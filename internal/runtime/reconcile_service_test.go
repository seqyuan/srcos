package runtime

import (
	"context"
	"testing"

	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
)

// newDeadServiceFixture writes a running service record and publishes its route,
// so a reconcile can be observed to withdraw both.
func newDeadServiceFixture(t *testing.T, configDir string, backend string) (*route.Table, *portpool.Pool) {
	t.Helper()
	inst := saveTestInstance(t, configDir, "alice-web-svc", "service", StateRunning)
	inst.Endpoint = "127.0.0.1:20123"
	inst.RoutePath = "/proxy/alice/web"
	inst.Backend = backend
	if err := SaveInstance(InstancePath(configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}
	routes := route.NewTable()
	if err := routes.Put(route.Entry{
		User: inst.User, Tool: inst.Tool, InstanceID: inst.ID,
		Path: inst.RoutePath, Target: route.Target{Host: "127.0.0.1", Port: 20123},
	}); err != nil {
		t.Fatal(err)
	}
	return routes, portpool.New(0, 0)
}

// A service that dies after startup must be settled on the next tick, not only
// at the next gateway restart — otherwise the record keeps saying "running"
// while every request gets a 502 (and the port is never released).
func TestReconcileServicesSettlesDeadService(t *testing.T) {
	configDir := t.TempDir()
	routes, ports := newDeadServiceFixture(t, configDir, "local")
	runner := NewRunner(Options{
		ConfigDir: configDir,
		Routes:    routes,
		Backends:  map[string]Backend{"local": &stubBackend{gone: true}},
	})
	runner.SetPorts(ports)

	orphaned, err := runner.ReconcileServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 1 || orphaned[0] != "alice-web-svc" {
		t.Fatalf("orphaned = %v, want the dead service", orphaned)
	}
	rec := loadTestInstance(t, configDir, "alice-web-svc")
	if rec.State != StateStopped {
		t.Fatalf("state = %s, want stopped", rec.State)
	}
	if rec.Error == "" {
		t.Fatal("the record must explain why it was settled")
	}
	if _, _, ok := routes.GetByPath("/proxy/alice/web"); ok {
		t.Fatal("the route of a dead service must be withdrawn")
	}
}

// A unit systemd is bringing back (`activating`, `auto-restart`) is not alive
// but is also not gone: orphaning it would delete the route of a service that
// is coming back. UnitGone exists precisely to tell the two apart.
func TestReconcileServicesLeavesARestartingServiceAlone(t *testing.T) {
	configDir := t.TempDir()
	routes, ports := newDeadServiceFixture(t, configDir, "local")
	runner := NewRunner(Options{
		ConfigDir: configDir,
		Routes:    routes,
		Backends:  map[string]Backend{"local": &stubBackend{alive: false, gone: false}},
	})
	runner.SetPorts(ports)

	orphaned, err := runner.ReconcileServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 0 {
		t.Fatalf("a restarting service must not be orphaned: %v", orphaned)
	}
	if rec := loadTestInstance(t, configDir, "alice-web-svc"); rec.State != StateRunning {
		t.Fatalf("a restarting service must keep running: %+v", rec)
	}
	if _, _, ok := routes.GetByPath("/proxy/alice/web"); !ok {
		t.Fatal("the route of a restarting service must stay published")
	}
}

// `external` is skipped outright: SRCOS does not own that process, so a
// momentarily unreachable upstream must not be recorded as a dead instance.
func TestReconcileServicesSkipsExternal(t *testing.T) {
	configDir := t.TempDir()
	routes, ports := newDeadServiceFixture(t, configDir, "external")
	runner := NewRunner(Options{
		ConfigDir: configDir,
		Routes:    routes,
		Backends:  map[string]Backend{"external": &stubBackend{gone: true}},
	})
	runner.SetPorts(ports)

	orphaned, err := runner.ReconcileServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 0 {
		t.Fatalf("external instances are not SRCOS's to orphan: %v", orphaned)
	}
	if rec := loadTestInstance(t, configDir, "alice-web-svc"); rec.State != StateRunning {
		t.Fatalf("an external instance must keep running: %+v", rec)
	}
}

// Terminal states are already decided, and a task is ReconcileTasks' business.
func TestReconcileServicesIgnoresDecidedAndTasks(t *testing.T) {
	configDir := t.TempDir()
	saveTestInstance(t, configDir, "alice-web-done", "service", StateStopped)
	saveTestInstance(t, configDir, "alice-demo-task", "task", StateRunning)
	runner := NewRunner(Options{
		ConfigDir: configDir,
		Backends:  map[string]Backend{"local": &stubBackend{gone: true}},
	})

	orphaned, err := runner.ReconcileServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 0 {
		t.Fatalf("nothing should have been touched: %v", orphaned)
	}
	if rec := loadTestInstance(t, configDir, "alice-demo-task"); rec.State != StateRunning {
		t.Fatalf("a task must not be settled here: %+v", rec)
	}
}

// The startup Reconcile adopts survivors but the periodic pass must not: a
// still-live service is left alone every tick.
func TestReconcileServicesLeavesLiveServiceAlone(t *testing.T) {
	configDir := t.TempDir()
	routes, ports := newDeadServiceFixture(t, configDir, "local")
	runner := NewRunner(Options{
		ConfigDir: configDir,
		Routes:    routes,
		Backends:  map[string]Backend{"local": &stubBackend{alive: true, gone: false}},
	})
	runner.SetPorts(ports)

	orphaned, err := runner.ReconcileServices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphaned) != 0 {
		t.Fatalf("a live service must be left alone: %v", orphaned)
	}
	rec := loadTestInstance(t, configDir, "alice-web-svc")
	if rec.State != StateRunning || !rec.EndedAt.IsZero() {
		// EndedAt must stay zero for a service that never ended.
		t.Fatalf("a live service must not be marked ended: %+v", rec)
	}
}
