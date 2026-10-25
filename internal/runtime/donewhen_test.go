package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// zeroHandle is a task that exits successfully immediately.
type zeroHandle struct{}

func (h *zeroHandle) Ref() string { return "zero" }
func (h *zeroHandle) Wait(context.Context) ExitStatus {
	return ExitStatus{Code: 0}
}
func (h *zeroHandle) Stop(context.Context) error     { return nil }
func (h *zeroHandle) Alive() bool                    { return false }
func (h *zeroHandle) Endpoint() (route.Target, bool) { return route.Target{}, false }
func (h *zeroHandle) WantEndpoint() bool             { return false }
func (h *zeroHandle) Command() []string              { return []string{"true"} }
func (h *zeroHandle) Limiter() string                { return "test" }

type zeroBackend struct{}

func (b *zeroBackend) Name() string { return "local" }
func (b *zeroBackend) Start(context.Context, StartRequest) (Handle, error) {
	return &zeroHandle{}, nil
}

// A tool-level completion probe is the normal case for a submitter: work.sh
// exits 0 after queueing child jobs, and the probe decides when the task is
// really done. It must never be replaced by "exit 0 means success".
func TestRunTaskHonoursToolLevelDoneWhen(t *testing.T) {
	old := probePollEvery
	probePollEvery = 10 * time.Millisecond
	defer func() { probePollEvery = old }()

	configDir := t.TempDir()
	tl := &tool.Tool{
		ID: "probe", Version: "0.1.0", Name: "Probe",
		Kind: tool.KindTask, Backend: tool.BackendLocal, Sandbox: tool.SandboxNone,
		Dir:       t.TempDir(),
		Resources: tool.Resources{CPU: 1, Memory: "64Mi", Walltime: "0:00:05"},
		Completion: &tool.Completion{
			DoneWhen:        &tool.DoneWhen{Type: tool.DoneWhenFileExists, Path: "/workspace/out/_SUCCESS"},
			DoneWhenTimeout: "0:00:01",
		},
	}
	runner := NewRunner(Options{
		ConfigDir: configDir, ToolsDir: t.TempDir(), User: "alice",
		Backends: map[string]Backend{"local": &zeroBackend{}},
	})
	loaded := &job.Loaded{ID: "j1", Dir: t.TempDir(), Job: &job.Job{Name: "j1", Command: []string{"true"}}}
	inst, err := runner.RunTask(context.Background(), tl, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != StateFailed {
		t.Fatalf("state = %s, want failed (the marker was never created)", inst.State)
	}
	if !strings.Contains(inst.Error, "doneWhen") {
		t.Fatalf("error = %q, want it to mention doneWhen", inst.Error)
	}
	if inst.DoneWhen == nil || inst.DoneWhenHost == "" {
		t.Fatalf("the effective probe was not recorded: %+v", inst)
	}
}

func TestReconcileSubmittedTaskWaitsForTheProbe(t *testing.T) {
	configDir := t.TempDir()
	host := filepath.Join(t.TempDir(), "marker")
	inst := &Instance{
		ID: "alice-probe-j1", User: "alice", Tool: "probe", Kind: string(tool.KindTask),
		State: StateSubmitted, Backend: "local", StartedAt: time.Now().UTC().Add(-time.Minute),
		DoneWhen:         &tool.DoneWhen{Type: tool.DoneWhenFileExists, Path: "/workspace/out/_SUCCESS"},
		DoneWhenHost:     host,
		DoneWhenDeadline: time.Now().UTC().Add(time.Minute),
	}
	if err := SaveInstance(InstancePath(configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(Options{ConfigDir: configDir})

	settled, err := runner.reconcileSubmittedTask(inst)
	if err != nil || settled {
		t.Fatalf("an unsatisfied probe was settled: settled=%v err=%v", settled, err)
	}
	if rec := loadTestInstance(t, configDir, inst.ID); rec.State != StateSubmitted {
		t.Fatalf("state = %s, want submitted while the probe is pending", rec.State)
	}

	if err := os.WriteFile(host, []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if settled, err := runner.reconcileSubmittedTask(inst); err != nil || !settled {
		t.Fatalf("satisfied probe: settled=%v err=%v", settled, err)
	}
	if rec := loadTestInstance(t, configDir, inst.ID); rec.State != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", rec.State)
	}
}

func TestReconcileSubmittedTaskFailsAtTheDeadline(t *testing.T) {
	configDir := t.TempDir()
	inst := &Instance{
		ID: "alice-probe-j2", User: "alice", Tool: "probe", Kind: string(tool.KindTask),
		State: StateSubmitted, Backend: "local", StartedAt: time.Now().UTC().Add(-time.Minute),
		DoneWhen:         &tool.DoneWhen{Type: tool.DoneWhenFileExists, Path: "/workspace/out/_SUCCESS"},
		DoneWhenHost:     filepath.Join(t.TempDir(), "never"),
		DoneWhenDeadline: time.Now().UTC().Add(-time.Second),
	}
	if err := SaveInstance(InstancePath(configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(Options{ConfigDir: configDir})
	settled, err := runner.reconcileSubmittedTask(inst)
	if err != nil || !settled {
		t.Fatalf("expired probe: settled=%v err=%v", settled, err)
	}
	if rec := loadTestInstance(t, configDir, inst.ID); rec.State != StateFailed {
		t.Fatalf("state = %s, want failed", rec.State)
	}
}

// A stop written by another goroutine is the authority while a probe is
// polling; the waiter must return the stopped record instead of overwriting it
// with success/failure.
func TestWaitForProbeStopsOnADeliberateStop(t *testing.T) {
	old := probePollEvery
	probePollEvery = 10 * time.Millisecond
	defer func() { probePollEvery = old }()
	configDir := t.TempDir()
	recordPath := InstancePath(configDir, "alice-probe-j3")
	if err := SaveInstance(recordPath, &Instance{ID: "alice-probe-j3", State: StateSubmitted}); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	go func() {
		time.Sleep(20 * time.Millisecond)
		once.Do(func() {
			_ = SaveInstance(recordPath, &Instance{ID: "alice-probe-j3", State: StateStopped})
		})
	}()
	runner := NewRunner(Options{ConfigDir: configDir})
	err := runner.waitForProbe(context.Background(),
		&tool.DoneWhen{Type: tool.DoneWhenFileExists, Path: "/nope"},
		filepath.Join(t.TempDir(), "never"), time.Minute, recordPath)
	if !errors.Is(err, errProbeStopped) {
		t.Fatalf("waitForProbe = %v, want errProbeStopped", err)
	}
}

// Healthcheck examples use both "60s" and H:MM:SS. Neither spelling may
// silently fall back to the default, and startup_grace is the real wait for a
// slow first load.
func TestHealthcheckSpecAcceptsBothSpellingsAndGrace(t *testing.T) {
	tl := &tool.Tool{Ingress: &tool.Ingress{Healthcheck: &tool.Healthcheck{
		Path: "/", Timeout: "60s", StartupGrace: "180s",
	}}}
	path, timeout, grace := healthcheckSpec(tl)
	if path != "/" || timeout != 60*time.Second || grace != 180*time.Second {
		t.Fatalf("path=%q timeout=%s grace=%s", path, timeout, grace)
	}
	tl.Ingress.Healthcheck.Timeout = "0:00:45"
	_, timeout, _ = healthcheckSpec(tl)
	if timeout != 45*time.Second {
		t.Fatalf("walltime timeout = %s, want 45s", timeout)
	}
}

// A degraded flow plan hands the tool host paths; reportOutputs/Artifacts must
// resolve those too, not call every one "unresolvable".
func TestResolveOutputAcceptsDegradedHostPaths(t *testing.T) {
	root := t.TempDir()
	spec := &sandbox.Spec{}
	if err := spec.Add(sandbox.Mount{HostPath: root, SandboxPath: sandbox.PathFlow, Mode: sandbox.ReadWrite, Origin: "builtin"}); err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(root, "run", "out")
	if err := os.MkdirAll(hostPath, 0o755); err != nil {
		t.Fatal(err)
	}
	got, _, ok := ResolveOutput(spec, hostPath, true)
	if !ok || got != hostPath {
		t.Fatalf("ResolveOutput(degraded) = %q,%v, want %q", got, ok, hostPath)
	}
	if _, _, ok := ResolveOutput(spec, hostPath, false); ok {
		t.Fatal("a host path must not resolve as a sandbox path")
	}
}
