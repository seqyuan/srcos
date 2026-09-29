package runtime

import (
	"context"
	"testing"
	"time"
)

// TestLifecycleConsumersDoNotMutateTheSharedSnapshot pins the read-only
// contract the scan tick relies on. The tick hands one snapshot to six
// consumers; a consumer that writes must re-read the record first
// (Runner.reloadForWrite), or a later consumer's SaveInstance would persist the
// earlier consumer's half-finished state.
//
// Before snapshot sharing each consumer listed for itself, so an in-place
// mutation was invisible to the others. With a shared snapshot it is not, and
// the assertions below fail if a consumer writes through the snapshot element
// (remove reloadForWrite and this test goes red).
func TestLifecycleConsumersDoNotMutateTheSharedSnapshot(t *testing.T) {
	configDir := t.TempDir()

	// A live service whose process is gone: ReconcileServices settles it.
	saveTestInstance(t, configDir, "alice-web-svc", "service", StateRunning)

	// A live service with a scheduler deadline: WarnExpiringLeases marks it.
	sge := saveTestInstance(t, configDir, "alice-sge-svc", "service", StateRunning)
	sge.Backend = "sge"
	sge.LeaseExpiresAt = time.Now().Add(time.Minute)
	if err := SaveInstance(InstancePath(configDir, sge.ID), sge); err != nil {
		t.Fatal(err)
	}

	// A running task whose process is gone: ReconcileTasks settles it.
	saveTestInstance(t, configDir, "alice-demo-run", "task", StateRunning)

	insts, err := ListInstances(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 3 {
		t.Fatalf("snapshot has %d elements, want 3", len(insts))
	}
	was := map[string]Instance{}
	for _, inst := range insts {
		was[inst.ID] = *inst
	}

	runner := NewRunner(Options{
		ConfigDir: configDir,
		Backends: map[string]Backend{
			"local": &stubBackend{alive: false, gone: true},
			// alive+not-gone: ReconcileServices leaves it alone, WarnExpiringLeases acts.
			"sge": &leaseWarnBackend{stubBackend: &stubBackend{alive: true, gone: false}, reason: "soon", due: true},
		},
	})

	if _, err := runner.ReconcileServices(context.Background(), insts); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.WarnExpiringLeases(context.Background(), insts); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.ReconcileTasks(context.Background(), insts); err != nil {
		t.Fatal(err)
	}

	// The records on disk must have been written...
	for id, want := range map[string]State{
		"alice-web-svc":  StateStopped,
		"alice-demo-run": StateStopped,
	} {
		if rec := loadTestInstance(t, configDir, id); rec.State != want {
			t.Fatalf("%s: on-disk state = %s, want %s (the consumer did not settle it)", id, rec.State, want)
		}
	}
	if rec := loadTestInstance(t, configDir, "alice-sge-svc"); rec.LeaseWarnedDeadline.IsZero() {
		t.Fatal("alice-sge-svc: on-disk LeaseWarnedDeadline was not written")
	}

	// ...but the shared snapshot must not have been touched.
	for _, inst := range insts {
		want := was[inst.ID]
		if inst.State != want.State ||
			inst.Error != want.Error ||
			inst.ExitCode != want.ExitCode ||
			!inst.EndedAt.Equal(want.EndedAt) ||
			!inst.LeaseWarnedDeadline.Equal(want.LeaseWarnedDeadline) {
			t.Fatalf("snapshot element %s was mutated by a consumer: state=%s error=%q endedAt=%v leaseWarned=%v (want state=%s)",
				inst.ID, inst.State, inst.Error, inst.EndedAt, inst.LeaseWarnedDeadline, want.State)
		}
	}
}
