package flowrun

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/runtime"
)

func saveRunForTest(t *testing.T, dataDir, user string, rec *flow.Run) {
	t.Helper()
	path := flow.RecordPath(dataDir, user, rec.ID)
	if err := os.MkdirAll(flow.RunDir(dataDir, user, rec.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := flow.SaveRun(dataDir, path, rec); err != nil {
		t.Fatal(err)
	}
}

func loadRunForTest(t *testing.T, dataDir, user, id string) *flow.Run {
	t.Helper()
	rec, err := flow.LoadRun(flow.RecordPath(dataDir, user, id))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReconcileInterruptedLeavesALiveOwnerAlone(t *testing.T) {
	dataDir := t.TempDir()
	start, ok := runtime.ProcessStartTime(os.Getpid())
	if !ok {
		t.Skip("no /proc process start time")
	}
	rec := &flow.Run{
		ID: "run-live", User: "alice", FlowID: "pipe", State: flow.RunRunning,
		StartedAt: time.Now().UTC(), OwnerPID: os.Getpid(), OwnerStart: start,
		Nodes: []flow.NodeState{{ID: "count", State: flow.NodeRunning}},
	}
	saveRunForTest(t, dataDir, "alice", rec)

	ids, err := ReconcileInterrupted(dataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("interrupted a live run: %v", ids)
	}
	got := loadRunForTest(t, dataDir, "alice", "run-live")
	if got.State != flow.RunRunning || got.Nodes[0].State != flow.NodeRunning {
		t.Fatalf("record changed: %+v", got)
	}
}

func TestReconcileInterruptedMarksADeadOwner(t *testing.T) {
	dataDir := t.TempDir()
	rec := &flow.Run{
		ID: "run-dead", User: "alice", FlowID: "pipe", State: flow.RunRunning,
		StartedAt: time.Now().UTC(), OwnerPID: 999999, OwnerStart: 1,
		Nodes: []flow.NodeState{
			{ID: "count", State: flow.NodeRunning},
			{ID: "qc", State: flow.NodePending},
		},
	}
	saveRunForTest(t, dataDir, "alice", rec)

	ids, err := ReconcileInterrupted(dataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "run-dead" {
		t.Fatalf("interrupted = %v", ids)
	}
	got := loadRunForTest(t, dataDir, "alice", "run-dead")
	if got.State != flow.RunInterrupted {
		t.Fatalf("state = %q", got.State)
	}
	if !strings.Contains(got.Error, "flow resume") {
		t.Fatalf("error = %q", got.Error)
	}
	if got.Nodes[0].State != flow.NodePending || got.Nodes[1].State != flow.NodePending {
		t.Fatalf("nodes = %+v", got.Nodes)
	}
	if got.EndedAt.IsZero() {
		t.Fatal("interrupted run has no ended_at")
	}
}

func TestReconcileInterruptedTreatsALegacyOwnerlessRunAsGone(t *testing.T) {
	dataDir := t.TempDir()
	rec := &flow.Run{
		ID: "run-old", User: "alice", FlowID: "pipe", State: flow.RunRunning,
		StartedAt: time.Now().UTC(),
	}
	saveRunForTest(t, dataDir, "alice", rec)
	ids, err := ReconcileInterrupted(dataDir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "run-old" {
		t.Fatalf("interrupted = %v", ids)
	}
}
