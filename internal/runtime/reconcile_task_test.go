package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubBackend answers the two questions ReconcileTasks asks: is the unit still
// there, and (when it is not) how did it end. It never starts anything, so
// these tests need no processes.
type stubBackend struct {
	alive bool
	// gone answers the stricter UnitGone question: a unit that systemd is still
	// managing (restarting) is not alive but is also not gone.
	gone       bool
	verdict    ExitStatus
	hasVerdict bool
}

func (b *stubBackend) TaskExit(ctx context.Context, inst *Instance) (ExitStatus, bool) {
	return b.verdict, b.hasVerdict
}

func (b *stubBackend) Name() string { return "local" }

func (b *stubBackend) Start(ctx context.Context, req StartRequest) (Handle, error) {
	return nil, errors.New("stub backend does not start units")
}

func (b *stubBackend) UnitAlive(ctx context.Context, inst *Instance) bool { return b.alive }

func (b *stubBackend) UnitGone(ctx context.Context, inst *Instance) bool { return b.gone }

// saveTestInstance writes a record the way a run would have.
func saveTestInstance(t *testing.T, configDir, id, kind string, state State) *Instance {
	t.Helper()
	inst := &Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      kind,
		JobName:   "demo",
		State:     state,
		Backend:   "local",
		StartedAt: time.Now().UTC().Add(-time.Minute),
	}
	if err := SaveInstance(InstancePath(configDir, id), inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func loadTestInstance(t *testing.T, configDir, id string) *Instance {
	t.Helper()
	inst, err := LoadInstance(InstancePath(configDir, id))
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// A task's verdict is written by the process that started it. When that process
// is gone, the record must stop claiming the task is still running — otherwise
// the UI lies and the user's quota stays consumed forever.
func TestReconcileTasksSettlesOrphans(t *testing.T) {
	configDir := t.TempDir()
	saveTestInstance(t, configDir, "alice-demo-gone", "task", StateRunning)

	// This backend cannot say how the run ended (no verdict file), so the record
	// must admit that rather than invent a state.
	runner := NewRunner(Options{ConfigDir: configDir, Backends: map[string]Backend{"local": &stubBackend{alive: false}}})
	adopted, settled, err := runner.ReconcileTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 0 || len(settled) != 1 || settled[0] != "alice-demo-gone" {
		t.Fatalf("adopted=%v settled=%v", adopted, settled)
	}
	rec := loadTestInstance(t, configDir, "alice-demo-gone")
	if rec.State != StateStopped {
		t.Fatalf("state = %s, want stopped", rec.State)
	}
	if rec.Error == "" || rec.Duration == "" {
		t.Fatalf("the record must explain itself: %+v", rec)
	}
	if rec.State.ConsumesResources() {
		t.Error("a settled task must stop holding its quota")
	}
}

// When the backend recorded how the run ended, the reconciler uses it: a task
// interrupted by a restart is settled with its real outcome, not an apology.
func TestReconcileTasksUsesTheBackendsVerdict(t *testing.T) {
	cases := []struct {
		name       string
		verdict    ExitStatus
		wantState  State
		wantCode   int
		wantErrHas string
	}{
		{"success", ExitStatus{Code: 0}, StateSucceeded, 0, ""},
		{"failure", ExitStatus{Code: 3, Err: errors.New("exited with code 3")}, StateFailed, 3, "code 3"},
		{"killed", ExitStatus{Code: -1, Err: errors.New("terminated by TERM")}, StateFailed, -1, "TERM"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			configDir := t.TempDir()
			saveTestInstance(t, configDir, "alice-demo-verdict", "task", StateRunning)
			backend := &stubBackend{alive: false, verdict: c.verdict, hasVerdict: true}
			runner := NewRunner(Options{ConfigDir: configDir, Backends: map[string]Backend{"local": backend}})

			adopted, settled, err := runner.ReconcileTasks(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(adopted) != 0 || len(settled) != 1 {
				t.Fatalf("adopted=%v settled=%v", adopted, settled)
			}
			rec := loadTestInstance(t, configDir, "alice-demo-verdict")
			if rec.State != c.wantState || rec.ExitCode != c.wantCode {
				t.Fatalf("record = state=%s code=%d error=%q", rec.State, rec.ExitCode, rec.Error)
			}
			if c.wantErrHas != "" && !strings.Contains(rec.Error, c.wantErrHas) {
				t.Fatalf("error = %q, want it to mention %q", rec.Error, c.wantErrHas)
			}
		})
	}
}

// The verdict file is systemd's format: "<EXIT_STATUS> <SERVICE_RESULT>".
func TestParseVerdict(t *testing.T) {
	cases := []struct {
		raw       string
		wantOK    bool
		wantCode  int
		wantErrIs string
	}{
		{"0 success", true, 0, ""},
		{"3 exit-code", true, 3, "exited with code 3"},
		{"TERM success", true, -1, "terminated by TERM"},
		{"137 oom-kill", true, 137, "oom-kill"},
		{"1 timeout", true, 1, "timeout"},
		{"", false, 0, ""},
		{"   ", false, 0, ""},
	}
	for _, c := range cases {
		got, ok := parseVerdict(c.raw)
		if ok != c.wantOK {
			t.Errorf("parseVerdict(%q) ok = %v, want %v", c.raw, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Code != c.wantCode {
			t.Errorf("parseVerdict(%q) code = %d, want %d", c.raw, got.Code, c.wantCode)
		}
		if c.wantErrIs != "" {
			if got.Err == nil || !strings.Contains(got.Err.Error(), c.wantErrIs) {
				t.Errorf("parseVerdict(%q) err = %v, want it to mention %q", c.raw, got.Err, c.wantErrIs)
			}
		}
	}
}

// A task that outlives the restart is left alone; a later tick settles it.
func TestReconcileTasksAdoptsSurvivors(t *testing.T) {
	configDir := t.TempDir()
	saveTestInstance(t, configDir, "alice-demo-alive", "task", StateRunning)

	runner := NewRunner(Options{ConfigDir: configDir, Backends: map[string]Backend{"local": &stubBackend{alive: true}}})
	adopted, settled, err := runner.ReconcileTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 0 || len(adopted) != 1 {
		t.Fatalf("adopted=%v settled=%v", adopted, settled)
	}
	if rec := loadTestInstance(t, configDir, "alice-demo-alive"); rec.State != StateRunning {
		t.Fatalf("a live task must keep running: %+v", rec)
	}
}

// Only states that had a process are reconciled: `pending` is a queue position
// the task queue owns, and terminal states are already decided.
func TestReconcileTasksIgnoresQueuedAndDecided(t *testing.T) {
	configDir := t.TempDir()
	saveTestInstance(t, configDir, "alice-demo-queued", "task", StatePending)
	saveTestInstance(t, configDir, "alice-demo-done", "task", StateSucceeded)
	saveTestInstance(t, configDir, "alice-web-svc", "service", StateRunning)

	runner := NewRunner(Options{ConfigDir: configDir, Backends: map[string]Backend{"local": &stubBackend{alive: false}}})
	adopted, settled, err := runner.ReconcileTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(adopted) != 0 || len(settled) != 0 {
		t.Fatalf("nothing should have been touched: adopted=%v settled=%v", adopted, settled)
	}
	if rec := loadTestInstance(t, configDir, "alice-demo-queued"); rec.State != StatePending {
		t.Fatalf("a queued task must stay queued: %+v", rec)
	}
	// Services are Reconcile's business (they own ports and routes).
	if rec := loadTestInstance(t, configDir, "alice-web-svc"); rec.State != StateRunning {
		t.Fatalf("a service must not be settled here: %+v", rec)
	}
}
