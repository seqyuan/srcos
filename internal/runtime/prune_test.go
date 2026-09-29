package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedInstance writes a record (and a log file, so pruning has something to
// remove beside it) and returns it.
func seedInstance(t *testing.T, configDir, id string, state State, started, ended time.Time) *Instance {
	t.Helper()
	p := PathsFor(configDir, "alice", "demo", id)
	if err := os.MkdirAll(filepath.Dir(p.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.LogPath, []byte("ran\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &Instance{
		ID:        InstanceID("alice", "demo", id),
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		State:     state,
		LogPath:   p.LogPath,
		StartedAt: started,
		EndedAt:   ended,
	}
	if err := SaveInstance(p.RecordPath, inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestPruneInstancesRemovesOnlyStaleTerminalRecords(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	old := seedInstance(t, configDir, "old", StateSucceeded,
		now.Add(-41*24*time.Hour), now.Add(-40*24*time.Hour))
	recent := seedInstance(t, configDir, "recent", StateFailed,
		now.Add(-2*time.Hour), now.Add(-time.Hour))
	// A long-running, non-terminal record must survive however old it is: its
	// record is live state, not history.
	running := seedInstance(t, configDir, "running", StateRunning,
		now.Add(-90*24*time.Hour), time.Time{})

	stale, err := StaleInstances(configDir, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != old.ID {
		t.Fatalf("stale = %d record(s), want only %s", len(stale), old.ID)
	}

	removed, err := PruneInstances(configDir, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].ID != old.ID {
		t.Fatalf("removed = %d record(s), want only %s", len(removed), old.ID)
	}

	// The stale record and its log are gone.
	if _, err := os.Stat(InstancePath(configDir, old.ID)); !os.IsNotExist(err) {
		t.Fatalf("stale record still on disk: %v", err)
	}
	if _, err := os.Stat(old.LogPath); !os.IsNotExist(err) {
		t.Fatalf("stale log still on disk: %v", err)
	}
	// The other two are untouched.
	for _, keep := range []*Instance{recent, running} {
		if _, err := os.Stat(InstancePath(configDir, keep.ID)); err != nil {
			t.Fatalf("record %s should have survived: %v", keep.ID, err)
		}
	}
}

func TestPruneInstancesKeepZeroIsANoOp(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := seedInstance(t, configDir, "old", StateSucceeded,
		now.Add(-365*24*time.Hour), now.Add(-300*24*time.Hour))

	removed, err := PruneInstances(configDir, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("keep=0 must remove nothing, removed %d", len(removed))
	}
	if _, err := os.Stat(InstancePath(configDir, old.ID)); err != nil {
		t.Fatalf("a keep=0 prune must not touch any record: %v", err)
	}
}

func TestListInstancesOrdersNewestFirst(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seedInstance(t, configDir, "a", StateSucceeded, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	seedInstance(t, configDir, "c", StateSucceeded, now.Add(-time.Hour), now.Add(-time.Hour))
	seedInstance(t, configDir, "b", StateSucceeded, now.Add(-2*time.Hour), now.Add(-2*time.Hour))

	insts, err := ListInstances(configDir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(insts))
	for _, i := range insts {
		got = append(got, i.ID)
	}
	want := []string{
		InstanceID("alice", "demo", "c"),
		InstanceID("alice", "demo", "b"),
		InstanceID("alice", "demo", "a"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
