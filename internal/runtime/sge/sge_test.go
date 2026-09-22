package sge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// fakeRunner records commands and replays canned output. This is what makes the
// SGE backend developable before a login node exists.
type fakeRunner struct {
	calls [][]string
	reply func(name string, args []string) (string, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.reply == nil {
		return "", nil
	}
	return f.reply(name, args)
}

func (f *fakeRunner) last() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

// fakeQsubAlwaysOK keeps tests that are not about submission from shelling out
// to a real scheduler.
func fakeQsubAlwaysOK() *fakeRunner {
	return &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		return "", os.ErrNotExist
	}}
}

func testConfig(t *testing.T, r Runner) Config {
	t.Helper()
	if r == nil {
		r = fakeQsubAlwaysOK()
	}
	dir := t.TempDir()
	return Config{
		SubmitDir:     filepath.Join(dir, "submit"),
		RendezvousDir: filepath.Join(dir, "rd"),
		Scheduler:     SchedulerDefaults{PE: "smp", PEAccounting: "cores"},
		PollEvery:     10 * time.Millisecond,
		Runner:        r,
	}
}

func testRequest(t *testing.T, cfg Config) runtime.StartRequest {
	t.Helper()
	toolDir := t.TempDir()
	manifest := `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: sge
sandbox: none
entry: work.sh
resources: {cpu: 4, memory: "8Gi", walltime: "0:10:00", queue: "sci.q"}
`
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tl, err := tool.Load(toolDir)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.StartRequest{
		Tool:       tl,
		Spec:       nil,
		Paths:      runtime.Paths{User: "alice", Tool: "demo", Workspace: t.TempDir(), JobID: "j1"},
		View:       runtime.PathView{Degraded: true, User: "alice"},
		Argv:       []string{"bash", "/tool/work.sh"},
		Cwd:        "/tmp",
		Env:        []string{"PATH=/usr/bin:/bin"},
		UnitName:   "srcos-alice-demo-j1",
		LogPath:    filepath.Join(t.TempDir(), "out.log"),
		InstanceID: "alice-demo-j1",
	}
}

func TestConfigValidate(t *testing.T) {
	cases := map[string]Config{
		"no submit dir":  {RendezvousDir: "/rd", Scheduler: SchedulerDefaults{PE: "smp"}},
		"no rendezvous":  {SubmitDir: "/sub", Scheduler: SchedulerDefaults{PE: "smp"}},
		"no PE":          {SubmitDir: "/sub", RendezvousDir: "/rd"},
		"bad accounting": {SubmitDir: "/sub", RendezvousDir: "/rd", Scheduler: SchedulerDefaults{PE: "smp", PEAccounting: "wat"}},
		"both channels":  {SubmitDir: "/sub", RendezvousDir: "/rd", Scheduler: SchedulerDefaults{PE: "smp"}, Tunnel: true, DirectDial: true},
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	ok := Config{SubmitDir: "/sub", RendezvousDir: "/rd", Scheduler: SchedulerDefaults{PE: "smp"}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// TestQsubArgsTranslatesResources is the heart of ADR-015: on a cluster the
// scheduler enforces resources, so the tool's declaration must become qsub
// flags. A mistake here is either an OOM-killed job or one that never starts.
func TestQsubArgsTranslatesResources(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	args := strings.Join(b.qsubArgs(req), " ")

	for _, want := range []string{
		"-pe smp 4",       // cpu -> slots
		"-l h_vmem=2G",    // 8Gi / 4 slots, per-slot
		"-l h_rt=0:10:00", // walltime, normalised
		"-q sci.q",        // the tool's queue
		"-N srcos-alice-demo-j1",
		"-j y",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("qsub args missing %q:\n%s", want, args)
		}
	}
}

func TestQsubArgsThreadsAccounting(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Scheduler.PEAccounting = "threads"
	cfg.Scheduler.ThreadsPerCore = 2
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	args := strings.Join(b.qsubArgs(req), " ")

	// 4 cores on a 2-thread site means 8 slots, and the per-slot memory has to
	// follow the slot count or the job over-reserves.
	if !strings.Contains(args, "-pe smp 8") {
		t.Errorf("expected 8 slots:\n%s", args)
	}
	if !strings.Contains(args, "-l h_vmem=1G") {
		t.Errorf("expected 8Gi/8 slots = 1G per slot:\n%s", args)
	}
}

func TestQsubArgsUsesDefaultsWhenToolIsSilent(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Scheduler.DefaultQueue = "all.q"
	cfg.Scheduler.Project = "PM-001"
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.Tool.Resources.Queue = ""
	args := strings.Join(b.qsubArgs(req), " ")

	if !strings.Contains(args, "-q all.q") {
		t.Errorf("default queue not applied:\n%s", args)
	}
	if !strings.Contains(args, "-P PM-001") {
		t.Errorf("project not applied:\n%s", args)
	}
}

func TestQsubArgsAppliesJobOverrides(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	// A job may lower resources; Job.EffectiveResources already enforces that,
	// and this checks the lowered value is what reaches the scheduler.
	req.Job = &job.Job{SchemaVersion: 1, Name: "x", Resources: &tool.Resources{CPU: 2}}
	args := strings.Join(b.qsubArgs(req), " ")

	if !strings.Contains(args, "-pe smp 2") {
		t.Fatalf("job override ignored:\n%s", args)
	}
}

func TestStartSubmitsAndRecordsJobID(t *testing.T) {
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("srcos-alice-demo-j1") has been submitted`, nil
		}
		return "", nil
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}

	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.Ref() != "4242" {
		t.Fatalf("job id = %q", h.Ref())
	}
	if h.Limiter() != "sge" {
		t.Fatalf("limiter = %q", h.Limiter())
	}

	// The job id and state land in the rendezvous directory, which is the only
	// thing the login node and the compute node share.
	rd := cfg.rendezvousFor("alice-demo-j1")
	if got, _ := ReadRendezvous(rd, FileJobID); got != "4242" {
		t.Fatalf("jobid = %q", got)
	}
	if got, _ := ReadRendezvous(rd, FileState); got != StateSubmitted {
		t.Fatalf("state = %q", got)
	}
	// The script must exist and be executable.
	scriptPath := filepath.Join(cfg.SubmitDir, "srcos-alice-demo-j1.sh")
	fi, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("job script: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Fatal("job script is not executable")
	}
}

func TestStartRejectsBadQsubOutput(t *testing.T) {
	fake := &fakeRunner{reply: func(string, []string) (string, error) { return "syntax error near line 3", nil }}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	if _, err := b.Start(context.Background(), testRequest(t, cfg)); err == nil {
		t.Fatal("a qsub failure must be an error, because the unit never started")
	}
}

func TestScriptPublishesStateAndExitCode(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	inner := []string{"/usr/bin/bwrap", "--ro-bind", "/usr", "/usr", "--", "bash", "/tool/work.sh"}
	script := b.renderScript(req, inner)

	// The bookkeeping that turns a shared directory into a control channel.
	for _, want := range []string{
		"#!/bin/bash",
		"_pub state starting",
		"_pub node \"$(hostname)\"",
		"_pub exit_code \"$_code\"",
		"exec '/usr/bin/bwrap' '--ro-bind' '/usr' '/usr' '--' 'bash' '/tool/work.sh'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// Writes must be atomic: the login node polls concurrently.
	if !strings.Contains(script, ".tmp\" && mv") {
		t.Error("rendezvous writes are not atomic")
	}
	// A task script must not advertise an endpoint.
	if strings.Contains(script, "_pub endpoint") {
		t.Error("a task script published an endpoint")
	}
}

func TestServiceScriptHandlesPortCollision(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	req.Tool.Kind = tool.KindService
	req.Tool.Ingress = &tool.Ingress{Port: 3838}
	script := b.renderScript(req, []string{"bash", "/tool/work.sh"})

	// Two service jobs can land on the same compute node, so a fixed port
	// would collide; the script prefers the declared one and falls back.
	for _, want := range []string{
		"DECLARED_PORT=3838",
		"_free_port",
		"_port_in_use",
		`_pub endpoint "$(hostname):$PORT"`,
		"_pub state running",
		"wait \"$CHILD\"",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("service script missing %q:\n%s", want, script)
		}
	}
	// Readiness must be observed before the endpoint is advertised.
	readyIdx := strings.Index(script, "connect_ex")
	endpointIdx := strings.Index(script, `_pub endpoint`)
	if readyIdx < 0 || endpointIdx < 0 || readyIdx > endpointIdx {
		t.Error("the endpoint is published before the tool is observed listening")
	}
}

func TestWaitReadsExitCodeFromRendezvous(t *testing.T) {
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		// qstat fails => the job is gone => Wait reads the recorded exit code.
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	rd := cfg.rendezvousFor("alice-demo-j1")
	if err := WriteRendezvous(rd, FileExitCode, "7"); err != nil {
		t.Fatal(err)
	}

	st := h.Wait(context.Background())
	if st.Code != 7 {
		t.Fatalf("exit code = %d (%v)", st.Code, st.Err)
	}
}

func TestWaitExplainsAMissingExitCode(t *testing.T) {
	// The job vanished without writing one: that is the signature of an
	// h_vmem or h_rt kill, and the message must say so rather than reporting a
	// bare failure.
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	st := h.Wait(context.Background())
	if st.Code != -1 || st.Err == nil {
		t.Fatalf("status = %+v", st)
	}
	for _, want := range []string{"h_vmem", "h_rt"} {
		if !strings.Contains(st.Err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, st.Err)
		}
	}
}

// TestEndpointWaitsForTheJob is the control-channel contract: the login node
// never guesses an address, it reads the one the job published.
func TestEndpointWaitsForTheJob(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	_ = req

	rd := cfg.rendezvousFor("alice-demo-j1")
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = WriteRendezvous(rd, FileEndpoint, "node07:41234")
	}()

	ep, ok := h.Endpoint()
	if !ok {
		t.Fatalf("no endpoint: %v", h.(*jobHandle).endpointErr)
	}
	if ep.Host != "node07" || ep.Port != 41234 {
		t.Fatalf("endpoint = %+v", ep)
	}
}

func TestStopReportsAlreadyGoneAsSuccess(t *testing.T) {
	// A qdel racing with natural completion is normal, not an error.
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qdel") {
			return "qdel: job 4242 not found", os.ErrNotExist
		}
		return `job 4242 submitted`, nil
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop on an already-gone job must succeed: %v", err)
	}
}

func TestUnitAliveTreatsSuspendedAsAlive(t *testing.T) {
	// ADR-015: a preempted job still holds its slot and may resume, so tearing
	// it down would destroy the user's work.
	suspended := `<job_info><job_list state="suspended"><JB_job_number>4242</JB_job_number><state>s</state></job_list></job_info>`
	fake := &fakeRunner{reply: func(string, []string) (string, error) { return suspended, nil }}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	if !b.UnitAlive(context.Background(), &runtime.Instance{BackendRef: "4242"}) {
		t.Fatal("a suspended job must count as alive")
	}

	gone := `<job_info></job_info>`
	fake2 := &fakeRunner{reply: func(string, []string) (string, error) { return gone, nil }}
	cfg2 := testConfig(t, fake2)
	b2 := &Backend{Config: cfg2}
	if b2.UnitAlive(context.Background(), &runtime.Instance{BackendRef: "4242"}) {
		t.Fatal("a job the scheduler does not know must not be alive")
	}
}

func TestParseQsubOutput(t *testing.T) {
	cases := map[string]string{
		`Your job 12345 ("x") has been submitted`: "12345",
		`12345`:                          "12345",
		`12345.1-1`:                      "12345.1-1",
		`your job 99 has been submitted`: "99",
	}
	for in, want := range cases {
		got, err := parseQsubOutput(in)
		if err != nil {
			t.Errorf("parseQsubOutput(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseQsubOutput(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "syntax error", "no numbers here"} {
		if _, err := parseQsubOutput(bad); err == nil {
			t.Errorf("parseQsubOutput(%q) should fail", bad)
		}
	}
}
