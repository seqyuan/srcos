package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stubBackend answers the only question ReconcileTasks asks: is the unit still
// there? It never starts anything, so these tests need no processes.
type stubBackend struct {
	alive bool
}

func (b *stubBackend) Name() string { return "local" }

func (b *stubBackend) Start(ctx context.Context, req StartRequest) (Handle, error) {
	return nil, errors.New("stub backend does not start units")
}

func (b *stubBackend) UnitAlive(ctx context.Context, inst *Instance) bool { return b.alive }

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
