package execute

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
)

// submitRaw writes a submission straight into the drop-box, the way the CLI or
// a tool's own UI does. It writes no instance record, so the job is exactly
// what a restart or an external submitter leaves behind.
func submitRaw(t *testing.T, f *fixture, user, toolID, name string, override ...*job.Job) (string, string) {
	t.Helper()
	j := &job.Job{SchemaVersion: 1, Name: name, Params: map[string]any{"word": "x"}}
	if len(override) > 0 && override[0] != nil {
		j = override[0]
	}
	id, dir, err := job.Submit(f.configDir, user, toolID, j)
	if err != nil {
		t.Fatal(err)
	}
	return id, dir
}

func TestQueueRunsWhatTheCLIQueued(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, _ := submitRaw(t, f, "alice", "demo", "queued")

	// Nothing has run before the queue's pass: this is the state after a
	// restart, or after `srcos job submit`.
	if _, err := os.Stat(runtime.InstancePath(f.configDir, runtime.InstanceID("alice", "demo", id))); !os.IsNotExist(err) {
		t.Fatalf("a raw submission must not have a record yet (%v)", err)
	}

	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d, want 1", n)
	}
	inst := waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", id), runtime.StateRunning)
	if inst.JobName != "queued" {
		t.Fatalf("record = %+v", inst)
	}
	f.backend.finishAll()
	waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", id), runtime.StateSucceeded)
}

func TestQueueRunsOldestFirst(t *testing.T) {
	f := newFixture(t, allowAll(t))
	newer, newerDir := submitRaw(t, f, "alice", "demo", "newer")
	older, olderDir := submitRaw(t, f, "alice", "demo", "older")

	// Make the submission order unambiguous: "older" was written an hour ago.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(olderDir, "job.json"), past, past); err != nil {
		t.Fatal(err)
	}
	_ = newerDir

	f.drain(t)
	// Only one may run (the queue is serial per user+tool), and it must be the
	// older one: a queue that runs its newest submission first is not a queue.
	inst := waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", older), runtime.StateRunning)
	if inst.JobName != "older" {
		t.Fatalf("ran %q, want the older submission", inst.JobName)
	}
	if rec, _ := runtime.LoadInstance(runtime.InstancePath(f.configDir, runtime.InstanceID("alice", "demo", newer))); rec != nil {
		t.Fatalf("the newer submission ran too early: %+v", rec)
	}
	f.releaseAndSettle(t)
}

func TestQueueIsSerialPerUserAndTool(t *testing.T) {
	f := newFixture(t, allowAll(t))
	submitRaw(t, f, "alice", "demo", "one")
	submitRaw(t, f, "alice", "demo", "two")

	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d runs of one (user, tool), want 1", n)
	}
	f.releaseAndSettle(t)
}

func TestQueueStopsAtTheWorkerCap(t *testing.T) {
	f := newFixture(t, allowAll(t))
	// Two different (user, tool) pairs, but a single worker: the cap, not the
	// pairing, is what limits a burst from many users.
	submitRaw(t, f, "alice", "demo", "a")
	submitRaw(t, f, "bob", "other", "b")

	q := NewQueue(f.Controller, QueueOptions{
		Workers: 1,
		Users:   func() []string { return []string{"alice", "bob"} },
		Log:     func(string, ...any) {},
	})
	if n := q.Tick(context.Background()); n != 1 {
		t.Fatalf("queue started %d with a cap of 1", n)
	}
	f.backend.finishAll()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && q.InFlight() > 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if n := q.Tick(context.Background()); n != 1 {
		t.Fatalf("queue started %d after the first finished, want 1", n)
	}
	// This test uses a local queue, not f.queue: wait on the one that actually
	// started the run before letting t.TempDir remove data/.
	waitQueueIdleFor(t, q)
	f.releaseAndSettle(t)
}

func TestQueueLeavesUngrantedWorkAlone(t *testing.T) {
	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "other", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, policy)
	id, _ := submitRaw(t, f, "alice", "demo", "revoked")

	if n := f.drain(t); n != 0 {
		t.Fatalf("queue started %d runs of an unauthorized tool", n)
	}
	// The submission is untouched, so granting the tool later runs it.
	if _, err := os.Stat(runtime.InstancePath(f.configDir, runtime.InstanceID("alice", "demo", id))); !os.IsNotExist(err) {
		t.Fatalf("an unauthorized submission must not get a record (%v)", err)
	}
	if job.Claimed(filepath.Join(f.configDir, "..", "data", "ws", "alice", "demo", "jobs", id)) {
		t.Error("an unauthorized submission must not be claimed")
	}
}

func TestQueueRecordsAnInvalidSubmissionAsFailed(t *testing.T) {
	f := newFixture(t, allowAll(t))
	// A hand-written job.json with a parameter the tool never declared: it can
	// never run, so it must be visible rather than retried forever.
	id, dir := submitRaw(t, f, "alice", "demo", "broken", &job.Job{
		SchemaVersion: 1, Name: "broken", Params: map[string]any{"nope": "x"},
	})

	if n := f.drain(t); n != 0 {
		t.Fatalf("queue started an invalid submission (%d)", n)
	}
	rec := waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", id), runtime.StateFailed)
	if !strings.Contains(rec.Error, "invalid submission") || !strings.Contains(rec.Error, "nope") {
		t.Fatalf("record = %+v", rec)
	}
	// The lease is released immediately for an invalid submission: the failed
	// record, not a permanent marker, is what stops it being rescanned.
	if job.Claimed(dir) {
		t.Error("an invalid submission should not leave a held lease")
	}
	// A second pass must not touch it again.
	if n := f.drain(t); n != 0 {
		t.Fatalf("queue restarted an invalid submission (%d)", n)
	}
}

// A flow node is a step in a DAG, not an independent queue item: the flow
// runner schedules it in dependency order (and has claimed it).
func TestQueueLeavesFlowNodesToTheFlowRunner(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, dir := submitRaw(t, f, "alice", "demo", "flow node", &job.Job{
		SchemaVersion: 1, Name: "flow node", Params: map[string]any{"word": "x"},
		Tags: map[string]string{"run": "pipe-1", "node": "tick"},
	})
	if n := f.drain(t); n != 0 {
		t.Fatalf("the queue ran a flow node (%d)", n)
	}
	if job.Claimed(dir) {
		t.Error("the queue must not even claim a flow node")
	}
	if _, err := os.Stat(runtime.InstancePath(f.configDir, runtime.InstanceID("alice", "demo", id))); !os.IsNotExist(err) {
		t.Fatal("a flow node must be left to its scheduler")
	}
}

func TestQueueSkipsClaimedJobs(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, dir := submitRaw(t, f, "alice", "demo", "taken")
	lease, err := job.Claim(dir)
	if err != nil || lease == nil {
		t.Fatalf("claim: %v %v", lease, err)
	}
	defer lease.Release()
	if n := f.drain(t); n != 0 {
		t.Fatalf("queue started a claimed job (%d)", n)
	}
	if _, err := os.Stat(runtime.InstancePath(f.configDir, runtime.InstanceID("alice", "demo", id))); !os.IsNotExist(err) {
		t.Fatal("a claimed job must be left to its runner")
	}
}

func TestQueueLeavesFinishedHistoryAlone(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, _ := submitRaw(t, f, "alice", "demo", "history")
	instID := runtime.InstanceID("alice", "demo", id)
	// A record in a terminal state means "attempted"; the daemon must not retry
	// it (a broken tool would spin forever).
	writeRecord(t, f.configDir, instID, runtime.StateSucceeded)

	if n := f.drain(t); n != 0 {
		t.Fatalf("queue restarted a finished job (%d)", n)
	}
	if f.backend.count() != 0 {
		t.Fatal("nothing should have been started")
	}
}

// writeRecord drops a minimal instance record for tests.
func writeRecord(t *testing.T, configDir, id string, state runtime.State) {
	t.Helper()
	path := runtime.InstancePath(configDir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runtime.SaveInstance(path, &runtime.Instance{
		ID: id, User: "alice", Tool: "demo", Kind: "task", State: state,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// A crashed runner releases its flock when the process exits. The submission
// must then be startable without any TTL or manual cleanup.
func TestQueueReclaimsAJobAfterACrashedClaim(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, dir := submitRaw(t, f, "alice", "demo", "crashed")
	lease, err := job.Claim(dir)
	if err != nil || lease == nil {
		t.Fatalf("claim = %v %v", lease, err)
	}
	// Simulate the process dying after the claim: close the descriptor (the
	// kernel drops the flock) without running the job.
	lease.Release()
	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d after a released claim, want 1", n)
	}
	waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", id), runtime.StateRunning)
	f.releaseAndSettle(t)
}

// The queue's publication marker is job.json. A half-written file (the O_EXCL
// fallback path on filesystems without hard links) must not start anything; the
// next tick, once the writer finished, must see the complete file.
func TestQueueWaitsForACompleteJobFile(t *testing.T) {
	f := newFixture(t, allowAll(t))
	id, dir := submitRaw(t, f, "alice", "demo", "half")
	path := filepath.Join(dir, "job.json")
	complete, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := f.drain(t); n != 0 {
		t.Fatalf("queue started %d from a half-written job.json", n)
	}
	if err := os.WriteFile(path, complete, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d after the job.json was completed, want 1", n)
	}
	waitInstance(t, f.configDir, runtime.InstanceID("alice", "demo", id), runtime.StateRunning)
	f.releaseAndSettle(t)
}
