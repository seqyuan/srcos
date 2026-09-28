package sge

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/sandbox"
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
		"-pe smp 4",    // cpu -> slots
		"-l h_vmem=2G", // 8Gi / 4 slots, per-slot
		"h_rt=0:10:00", // walltime, normalised (kept in the same -l list)
		"-q sci.q",     // the tool's queue
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
		"'/usr/bin/bwrap' '--ro-bind' '/usr' '/usr' '--' 'bash' '/tool/work.sh'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// The task body must not exec: that would replace the shell and skip the
	// exit-code bookkeeping below it.
	if strings.Contains(script, "exec '/usr/bin/bwrap'") {
		t.Errorf("task body used exec, so the exit code would never be published:\n%s", script)
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

// ─────────────────────────────────────────────────────────────────────────
// SRCOS_PORT threading (services choose their port on the compute node)
// ─────────────────────────────────────────────────────────────────────────

func TestWithPortPlaceholderReplacesThePoolPort(t *testing.T) {
	got := withPortPlaceholder([]string{"PATH=/bin", "SRCOS_PORT=54321", "HOME=/h"})
	for _, kv := range got {
		if strings.HasPrefix(kv, "SRCOS_PORT=") && kv != "SRCOS_PORT="+portPlaceholder {
			t.Fatalf("pool port survived: %v", got)
		}
	}
	if !strings.Contains(strings.Join(got, " "), "SRCOS_PORT="+portPlaceholder) {
		t.Fatalf("placeholder not added: %v", got)
	}
	// The placeholder must appear exactly once, whatever the caller passed.
	n := 0
	for _, kv := range got {
		if strings.HasPrefix(kv, "SRCOS_PORT=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("SRCOS_PORT appears %d times: %v", n, got)
	}
}

func TestShellJoinPortExpandsEnvI(t *testing.T) {
	inner := []string{"env", "-i", "SRCOS_PORT=" + portPlaceholder, "bash", "/tool/work.sh"}
	got := shellJoinInner(inner)
	if !strings.Contains(got, `SRCOS_PORT="$PORT"`) {
		t.Fatalf("env -i form not expanded: %s", got)
	}
	if strings.Contains(got, portPlaceholder) {
		t.Fatalf("placeholder leaked: %s", got)
	}
}

func TestShellJoinPortExpandsBwrapSetenv(t *testing.T) {
	inner := []string{"bwrap", "--setenv", "SRCOS_PORT", portPlaceholder, "--", "bash", "/tool/work.sh"}
	got := shellJoinInner(inner)
	if !strings.Contains(got, `'SRCOS_PORT' "$PORT"`) {
		t.Fatalf("bwrap --setenv form not expanded: %s", got)
	}
	if strings.Contains(got, portPlaceholder) {
		t.Fatalf("placeholder leaked: %s", got)
	}
}

// TestStartThreadsTheComputePortToTheTool is the end-to-end form: the script
// written for the cluster must expand the port, never the login node's pool
// value and never the literal string "$PORT".
func TestStartThreadsTheComputePortToTheTool(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	req.Tool.Kind = tool.KindService
	req.Tool.Ingress = &tool.Ingress{Port: 3838}
	req.Env = append(req.Env, "SRCOS_PORT=54321")
	if _, err := b.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.SubmitDir, req.UnitName+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.Contains(script, `SRCOS_PORT="$PORT"`) {
		t.Errorf("the tool is never given the port it must bind:\n%s", script)
	}
	if strings.Contains(script, "SRCOS_PORT=54321") {
		t.Errorf("the login node's pool port leaked into a compute-node job:\n%s", script)
	}
	if strings.Contains(script, portPlaceholder) {
		t.Errorf("placeholder leaked into the rendered script:\n%s", script)
	}
	if strings.Contains(script, `SRCOS_PORT='$PORT'`) {
		t.Errorf("SRCOS_PORT is a literal, not an expansion:\n%s", script)
	}
}

func TestServiceScriptWithoutPython3StillChecksReadiness(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	req.Tool.Kind = tool.KindService
	req.Tool.Ingress = &tool.Ingress{Port: 3838}
	script := b.renderScript(req, []string{"bash", "/tool/work.sh"})

	// With no python3 there is no bind-probe, but readiness must still be
	// observed (via bash /dev/tcp) instead of being skipped silently, and an
	// empty port must fail loudly rather than publish "host:".
	for _, want := range []string{
		`/dev/tcp/127.0.0.1/$PORT`,
		"_ready",
		"no usable port",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "  else\n    break\n  fi\n  sleep 0.5") {
		t.Error("readiness is still skipped when python3 is absent")
	}
}

func TestScriptRecordsTheCanonicalJobIDField(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	script := b.renderScript(testRequest(t, cfg), []string{"bash", "/tool/work.sh"})
	if !strings.Contains(script, "_pub "+FileJobID+" ") {
		t.Errorf("script does not publish %q:\n%s", FileJobID, script)
	}
	if strings.Contains(script, "_pub job_id ") {
		t.Errorf("script still publishes the non-canonical job_id field:\n%s", script)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Wait robustness against transient qstat failures
// ─────────────────────────────────────────────────────────────────────────

// runningJob is a minimal positive qstat -xml document for job 4242.
const runningJob = `<job_info><job_list><JB_job_number>4242</JB_job_number><state>r</state></job_list></job_info>`

func TestWaitIgnoresATransientQstatBlip(t *testing.T) {
	// One miss, then the scheduler answers "running" again: the job must not be
	// declared finished on the strength of that single miss. It takes three
	// consecutive misses (or a terminal rendezvous state) to conclude it is gone.
	qstatCalls := 0
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		qstatCalls++
		switch qstatCalls {
		case 1:
			return "", os.ErrNotExist // blip: job unknown, but it is not gone
		case 2, 3:
			return runningJob, nil
		default:
			return "", os.ErrNotExist // now really gone
		}
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRendezvous(cfg.rendezvousFor("alice-demo-j1"), FileExitCode, "0"); err != nil {
		t.Fatal(err)
	}

	st := h.Wait(context.Background())
	if st.Code != 0 {
		t.Fatalf("exit code = %d (%v)", st.Code, st.Err)
	}
	// Calls 4,5,6 are the three consecutive misses; the first blip must have
	// reset rather than terminated the loop.
	if qstatCalls != 6 {
		t.Fatalf("qstat called %d times, want 6 (a blip ended the wait early?)", qstatCalls)
	}
}

func TestWaitTrustsATerminalRendezvousStateImmediately(t *testing.T) {
	qstatCalls := 0
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		qstatCalls++
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	rd := cfg.rendezvousFor("alice-demo-j1")
	// The job wrote its own testimony before leaving the scheduler's view.
	if err := WriteRendezvous(rd, FileState, StateExited); err != nil {
		t.Fatal(err)
	}
	if err := WriteRendezvous(rd, FileExitCode, "3"); err != nil {
		t.Fatal(err)
	}

	st := h.Wait(context.Background())
	if st.Code != 3 {
		t.Fatalf("exit code = %d (%v)", st.Code, st.Err)
	}
	if qstatCalls != 1 {
		t.Fatalf("qstat called %d times, want 1 (a terminal state should settle it at once)", qstatCalls)
	}
}

func TestParseLeadingIntRejectsNonNumeric(t *testing.T) {
	if _, err := parseLeadingInt("smp"); err == nil {
		t.Fatal("a non-numeric slot count must not parse as 0")
	}
	if n, err := parseLeadingInt("4"); err != nil || n != 4 {
		t.Fatalf("parseLeadingInt(4) = %d, %v", n, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// The data channel (ssh -L) for services
// ─────────────────────────────────────────────────────────────────────────

type fakePortAllocator struct {
	acquired []string
	released []int
	next     int
}

func (f *fakePortAllocator) Acquire(owner string) (int, error) {
	f.acquired = append(f.acquired, owner)
	f.next++
	return 41000 + f.next, nil
}
func (f *fakePortAllocator) Release(port int) { f.released = append(f.released, port) }

type fakeTunnelRuntime struct {
	alive   bool
	stopped bool
}

func (f *fakeTunnelRuntime) Alive() bool                { return f.alive }
func (f *fakeTunnelRuntime) Stop(context.Context) error { f.stopped = true; return nil }

func TestServiceEndpointStartsATunnel(t *testing.T) {
	runner := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qsub") {
			return `Your job 4242 ("x") has been submitted`, nil
		}
		if strings.HasSuffix(name, "qdel") {
			return "", nil
		}
		return "", os.ErrNotExist
	}}
	ports := &fakePortAllocator{}
	tunRuntime := &fakeTunnelRuntime{alive: true}
	var built *Tunnel

	cfg := testConfig(t, runner)
	cfg.Tunnel = true
	cfg.Ports = ports
	cfg.NewTunnel = func(_ context.Context, tun *Tunnel) (TunnelRuntime, error) {
		built = tun
		return tunRuntime, nil
	}
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	req.Tool.Kind = tool.KindService
	req.Tool.Ingress = &tool.Ingress{Port: 3838}
	h, err := b.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	rd := cfg.rendezvousFor("alice-demo-j1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = WriteRendezvous(rd, FileEndpoint, "node07:41234")
	}()

	ep, ok := h.Endpoint()
	if !ok {
		t.Fatalf("no endpoint: %v", h.(*jobHandle).endpointErr)
	}
	// The gateway may only route to loopback, so the compute node's address is
	// replaced by the tunnel's local port.
	if ep.Host != "127.0.0.1" || ep.Port != 41001 {
		t.Fatalf("endpoint = %+v, want loopback:41001", ep)
	}
	if built == nil || built.Node != "node07" || built.LocalPort != 41001 || built.RemotePort != 41234 {
		t.Fatalf("tunnel = %+v", built)
	}
	if built.RemoteHost != "127.0.0.1" {
		t.Errorf("the tool binds loopback on the compute node, so RemoteHost must be 127.0.0.1: %+v", built)
	}
	if len(ports.acquired) != 1 {
		t.Fatalf("port allocations = %v", ports.acquired)
	}

	// A dead data channel makes the service unreachable even while the job runs.
	tunRuntime.alive = false
	if h.Alive() {
		t.Error("a dead tunnel must make the service not alive")
	}
	tunRuntime.alive = true

	if err := h.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !tunRuntime.stopped {
		t.Error("Stop did not stop the tunnel")
	}
	if len(ports.released) != 1 || ports.released[0] != 41001 {
		t.Errorf("tunnel port not released: %v", ports.released)
	}
}

func TestServiceWithoutAPortAllocatorFailsClearly(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Tunnel = true
	// Ports left nil: no allocator is wired on this host.
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.WantEndpoint = true
	req.Tool.Kind = tool.KindService
	req.Tool.Ingress = &tool.Ingress{Port: 3838}
	h, err := b.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	rd := cfg.rendezvousFor("alice-demo-j1")
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = WriteRendezvous(rd, FileEndpoint, "node07:41234")
	}()
	if _, ok := h.Endpoint(); ok {
		t.Fatal("a tunnel without a port allocator must not report an endpoint")
	}
	if msg := h.(*jobHandle).endpointErr; msg == nil || !strings.Contains(msg.Error(), "port allocator") {
		t.Fatalf("error does not explain the missing allocator: %v", msg)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// qstat query form (found wrong against a real cluster)
// ─────────────────────────────────────────────────────────────────────────

// realQstatDocument is the standard `qstat -xml` shape from SGE 8.1.9.
const realQstatDocument = `<?xml version='1.0'?>
<job_info xmlns:xsd="http://example/qstat.xsd">
  <queue_info>
    <job_list state="running">
      <JB_job_number>5418684</JB_job_number>
      <JAT_prio>0.50500</JAT_prio>
      <JB_name>srcos_sleep</JB_name>
      <JB_owner>yuanzan</JB_owner>
      <state>r</state>
      <JAT_start_time>2026-09-28T14:58:15</JAT_start_time>
      <queue_name>gpu.q@node060-gpu</queue_name>
      <slots>1</slots>
    </job_list>
  </queue_info>
  <job_info>
    <job_list state="pending">
      <JB_job_number>5418700</JB_job_number>
      <JB_name>srcos_queued</JB_name>
      <JB_owner>yuanzan</JB_owner>
      <state>qw</state>
      <queue_name>all.q@node000</queue_name>
      <slots>4</slots>
    </job_list>
  </job_info>
</job_info>`

func TestParseRealQstatDocument(t *testing.T) {
	jobs, err := ParseQstatXML(realQstatDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("parsed %d jobs, want 2", len(jobs))
	}
	run, ok := FindJob(jobs, "5418684")
	if !ok || !run.State.Running || run.Queue != "gpu.q@node060-gpu" || run.Slots != 1 {
		t.Fatalf("running job = %+v (ok=%v)", run, ok)
	}
	pend, ok := FindJob(jobs, "5418700")
	if !ok || !pend.State.Pending || pend.State.Running || pend.Slots != 4 {
		t.Fatalf("pending job = %+v (ok=%v)", pend, ok)
	}
}

// TestQueryStateUsesPlainQstat guards a bug only a real cluster exposed: the
// `qstat -xml -j <id>` form returns a <detailed_job_info> document with no
// <job_list>, so the parser always answered "unknown" and Wait declared every
// job finished the moment it looked.
func TestQueryStateUsesPlainQstat(t *testing.T) {
	var qstatArgs []string
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		switch {
		case strings.HasSuffix(name, "qsub"):
			return `Your job 4242 ("x") has been submitted`, nil
		case strings.HasSuffix(name, "qstat"):
			qstatArgs = append([]string(nil), args...)
			return realQstatDocument, nil
		}
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	b := &Backend{Config: cfg}
	h, err := b.Start(context.Background(), testRequest(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.(*jobHandle).queryState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(qstatArgs, " "); got != "-xml" {
		t.Fatalf("qstat invoked as %q, want %q", got, "-xml")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// h_rt renewal (qalter)
// ─────────────────────────────────────────────────────────────────────────

func TestUnitRenewOnlyWhenDue(t *testing.T) {
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qalter") {
			return "", nil
		}
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	cfg.Scheduler.RenewBefore = 10 * time.Minute
	cfg.Scheduler.RenewFor = time.Hour
	b := &Backend{Config: cfg}
	inst := &runtime.Instance{BackendRef: "4242", StartedAt: time.Now(), LeaseExpiresAt: time.Now().Add(time.Hour)}
	// The tool declares memory, so a renewal must repeat h_vmem: `qalter -l`
	// replaces the resource list and the scheduler refuses to drop a consumable
	// a running job already holds (observed on a real cluster).
	tl := &tool.Tool{Resources: tool.Resources{CPU: 4, Memory: "8Gi", Walltime: "1:00:00"}}

	// Far from the ceiling: no qalter.
	if did, err := b.UnitRenew(context.Background(), inst, tl); err != nil || did {
		t.Fatalf("premature renewal: did=%v err=%v", did, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("qalter called early: %v", fake.calls)
	}

	// Within the window: renew.
	inst.LeaseExpiresAt = time.Now().Add(5 * time.Minute)
	did, err := b.UnitRenew(context.Background(), inst, tl)
	if err != nil || !did {
		t.Fatalf("renewal did not fire: did=%v err=%v", did, err)
	}
	joined := strings.Join(fake.last(), " ")
	if !strings.Contains(joined, "qalter") || !strings.Contains(joined, "-l") || !strings.Contains(joined, "h_rt=") {
		t.Fatalf("qalter args = %v", fake.last())
	}
	if !strings.Contains(joined, "h_vmem=2G") {
		t.Fatalf("renewal dropped h_vmem, which a running job's qalter must repeat: %v", fake.last())
	}
	if !inst.LeaseExpiresAt.After(time.Now().Add(30 * time.Minute)) {
		t.Fatalf("lease not moved forward: %v", inst.LeaseExpiresAt)
	}

	// A second tick in the same window must not renew again.
	if did, _ := b.UnitRenew(context.Background(), inst, tl); did {
		t.Fatal("renewed twice for the same window")
	}
}

func TestUnitRenewDisabledByDefault(t *testing.T) {
	fake := &fakeRunner{}
	cfg := testConfig(t, fake) // RenewBefore is zero
	b := &Backend{Config: cfg}
	inst := &runtime.Instance{BackendRef: "1", StartedAt: time.Now(), LeaseExpiresAt: time.Now()}
	if did, err := b.UnitRenew(context.Background(), inst, &tool.Tool{}); err != nil || did {
		t.Fatalf("renewal ran while disabled: did=%v err=%v", did, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("qalter called while disabled: %v", fake.calls)
	}
}

func TestFormatWalltimeSeconds(t *testing.T) {
	cases := map[int64]string{0: "0:00:00", 60: "0:01:00", 3661: "1:01:01", 90000: "25:00:00"}
	for secs, want := range cases {
		if got := formatWalltimeSeconds(secs); got != want {
			t.Errorf("formatWalltimeSeconds(%d) = %q, want %q", secs, got, want)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Reattach after a gateway restart
// ─────────────────────────────────────────────────────────────────────────

func TestReattachUnitAdoptsASurvivingTunnel(t *testing.T) {
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qdel") {
			return "", nil
		}
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	cfg.Tunnel = true
	b := &Backend{Config: cfg}

	// A live loopback listener stands in for the ssh forward that outlived the
	// gateway (the child is in its own process group and is not killed with it).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	inst := &runtime.Instance{ID: "svc1", BackendRef: "4242", Endpoint: fmt.Sprintf("127.0.0.1:%d", port)}
	if err := WriteRendezvous(cfg.rendezvousFor(inst.ID), FileEndpoint, "node07:41234"); err != nil {
		t.Fatal(err)
	}
	if err := b.ReattachUnit(context.Background(), inst); err != nil {
		t.Fatalf("a surviving tunnel must be adopted: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("an adopted instance was stopped: %v", fake.calls)
	}
}

func TestReattachUnitSettlesADeadTunnel(t *testing.T) {
	fake := &fakeRunner{reply: func(name string, args []string) (string, error) {
		if strings.HasSuffix(name, "qdel") {
			return "", nil
		}
		return "", os.ErrNotExist
	}}
	cfg := testConfig(t, fake)
	cfg.Tunnel = true
	b := &Backend{Config: cfg}

	// A port with nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	inst := &runtime.Instance{ID: "svc1", BackendRef: "4242", Endpoint: fmt.Sprintf("127.0.0.1:%d", port)}
	if err := WriteRendezvous(cfg.rendezvousFor(inst.ID), FileEndpoint, "node07:41234"); err != nil {
		t.Fatal(err)
	}
	if err := b.ReattachUnit(context.Background(), inst); err == nil {
		t.Fatal("a dead tunnel must not be adopted")
	}
	// The job must be stopped so it does not keep a slot with no route to it.
	if len(fake.calls) == 0 || !strings.HasSuffix(fake.calls[len(fake.calls)-1][0], "qdel") {
		t.Fatalf("the unadoptable job was not stopped: %v", fake.calls)
	}
}

func TestReattachUnitIsANoOpWithoutATunnel(t *testing.T) {
	fake := &fakeRunner{}
	cfg := testConfig(t, fake)
	cfg.Tunnel = false
	b := &Backend{Config: cfg}
	if err := b.ReattachUnit(context.Background(), &runtime.Instance{ID: "x", BackendRef: "1"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("unexpected calls: %v", fake.calls)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// h_rt lease warning
// ─────────────────────────────────────────────────────────────────────────

func TestLeaseWarningOnlyWhenClose(t *testing.T) {
	cfg := testConfig(t, nil)
	cfg.Scheduler.WarnBefore = 10 * time.Minute
	cfg.Scheduler.RenewBefore = 5 * time.Minute
	b := &Backend{Config: cfg}

	if _, due := b.LeaseWarning(&runtime.Instance{}); due {
		t.Fatal("warned for an instance with no lease")
	}
	if _, due := b.LeaseWarning(&runtime.Instance{LeaseExpiresAt: time.Now().Add(time.Hour)}); due {
		t.Fatal("warned far from the ceiling")
	}
	msg, due := b.LeaseWarning(&runtime.Instance{LeaseExpiresAt: time.Now().Add(5 * time.Minute)})
	if !due || !strings.Contains(msg, "h_rt") || !strings.Contains(msg, "renewal is enabled") {
		t.Fatalf("msg=%q due=%v", msg, due)
	}

	// With renewal off, the message must say the scheduler will end the job.
	cfg.Scheduler.RenewBefore = 0
	b = &Backend{Config: cfg}
	msg, due = b.LeaseWarning(&runtime.Instance{LeaseExpiresAt: time.Now().Add(5 * time.Minute)})
	if !due || !strings.Contains(msg, "renewal is disabled") {
		t.Fatalf("msg=%q due=%v", msg, due)
	}
}

func TestValidateRejectsWarnBeforeSmallerThanRenewBefore(t *testing.T) {
	cfg := Config{
		SubmitDir: "/s", RendezvousDir: "/r",
		Scheduler: SchedulerDefaults{PE: "smp", RenewBefore: 20 * time.Minute, WarnBefore: 10 * time.Minute},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("warn_before < renew_before would never fire; expected a validation error")
	}
}

// The container runtime is resolved on the compute node, so the argv carries a
// placeholder that the rendered script turns into $SRCOS_CONTAINER_BIN.
func TestShellJoinInnerExpandsTheContainerRuntime(t *testing.T) {
	got := shellJoinInner([]string{sandbox.ContainerPlaceholder, "exec", "--contain", "/img/x.sif", "true"})
	if !strings.Contains(got, `"$SRCOS_CONTAINER_BIN" 'exec' '--contain'`) {
		t.Fatalf("container placeholder not expanded: %s", got)
	}
	if strings.Contains(got, sandbox.ContainerPlaceholder) {
		t.Fatalf("placeholder leaked: %s", got)
	}
}

func TestApptainerScriptResolvesTheRuntimeOnTheComputeNode(t *testing.T) {
	cfg := testConfig(t, nil)
	b := &Backend{Config: cfg}
	req := testRequest(t, cfg)
	req.Tool.Sandbox = tool.SandboxApptainer
	req.Tool.Image = "/shared/img/x.sif"
	script := b.renderScript(req, []string{sandbox.ContainerPlaceholder, "exec", "/shared/img/x.sif", "true"})
	for _, want := range []string{
		`SRCOS_CONTAINER_BIN="$(command -v apptainer || command -v singularity || true)"`,
		`"$SRCOS_CONTAINER_BIN" 'exec'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// A bwrap/none tool must not carry the container resolver.
	req.Tool.Sandbox = tool.SandboxNone
	plain := b.renderScript(req, []string{"bash", "/tool/work.sh"})
	if strings.Contains(plain, "SRCOS_CONTAINER_BIN") {
		t.Errorf("container resolver emitted for a non-container tool:\n%s", plain)
	}
}
