package inspect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
)

// logFixture writes an instance record (and nothing else) so FollowLogs has
// something to follow, and returns the log path the record points at.
func logFixture(t *testing.T, state runtime.State) (*Reader, string, string) {
	t.Helper()
	configDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "job.log")
	id := "alice-demo-job1"
	inst := &runtime.Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   "job1",
		State:     state,
		LogPath:   logPath,
		StartedAt: time.Now().UTC(),
	}
	if err := runtime.SaveInstance(runtime.InstancePath(configDir, id), inst); err != nil {
		t.Fatal(err)
	}
	return &Reader{ConfigDir: configDir}, id, logPath
}

func setInstanceState(t *testing.T, r *Reader, id string, state runtime.State) {
	t.Helper()
	path := runtime.InstancePath(r.ConfigDir, id)
	inst, err := runtime.LoadInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	inst.State = state
	if err := runtime.SaveInstance(path, inst); err != nil {
		t.Fatal(err)
	}
}

// eventSink collects follower events for assertion.
type eventSink struct {
	mu     sync.Mutex
	events []LogEvent
}

func (s *eventSink) emit(ev LogEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *eventSink) snapshot() []LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]LogEvent(nil), s.events...)
}

// lines returns the line payloads seen so far.
func (s *eventSink) lines() []string {
	var out []string
	for _, ev := range s.snapshot() {
		if ev.Kind == LogEventLine {
			out = append(out, ev.Line)
		}
	}
	return out
}

// startFollow runs a follower in the background. cancel is always safe to call:
// a test that ends the stream with a terminal state still uses it in cleanup so
// no goroutine outlives its fixtures.
func startFollow(t *testing.T, r *Reader, needle string, tail int) (*eventSink, chan error, context.CancelFunc) {
	t.Helper()
	sink := &eventSink{}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.FollowLogs(ctx, "alice", needle, tail, sink.emit) }()
	t.Cleanup(cancel)
	return sink, errc, cancel
}

// waitFor polls a condition, so a test never depends on a sleep being long
// enough (and never takes a second longer than it must).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitDone(t *testing.T, errc chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("the follower did not return")
		return nil
	}
}

// fastFollow makes the polling loop quick for this reader, without touching
// package state: a global would race with a goroutine from a previous test that
// has not returned yet.
func fastFollow(r *Reader) {
	r.FollowInterval = 2 * time.Millisecond
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestFollowLogsReplaysThenStreamsAndEnds(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	if err := os.WriteFile(logPath, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sink, errc, _ := startFollow(t, r, id, 100)
	waitFor(t, "the replayed tail", func() bool { return len(sink.lines()) >= 2 })

	// Append a third line: it must arrive while the instance is still running.
	appendFile(t, logPath, "three\n")
	waitFor(t, "the appended line", func() bool { return len(sink.lines()) >= 3 })

	// A terminal state ends the stream, and the state is reported.
	setInstanceState(t, r, id, runtime.StateSucceeded)
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}

	got := sink.lines()
	if len(got) != 3 || got[0] != "one" || got[1] != "two" || got[2] != "three" {
		t.Fatalf("lines = %q", got)
	}
	events := sink.snapshot()
	last := events[len(events)-1]
	if last.Kind != LogEventEnd || last.State != "succeeded" {
		t.Fatalf("last event = %+v", last)
	}
}

func TestFollowLogsWaitsForALogThatDoesNotExistYet(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StatePending)
	fastFollow(r)

	sink, _, cancel := startFollow(t, r, id, 100)
	// Poll a few times with no file at all: the stream must not give up, because
	// the task simply has not started.
	time.Sleep(20 * time.Millisecond)
	if got := sink.lines(); len(got) != 0 {
		t.Fatalf("lines before the log existed = %q", got)
	}

	appendFile(t, logPath, "late\n")
	waitFor(t, "the log to appear", func() bool { return len(sink.lines()) >= 1 })
	if got := sink.lines()[0]; got != "late" {
		t.Fatalf("line = %q", got)
	}
	cancel()
}

func TestFollowLogsHoldsBackAPartialLine(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	// A line without its newline must not be emitted until the newline arrives,
	// otherwise a client would see half a line as a line of its own.
	if err := os.WriteFile(logPath, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink, errc, _ := startFollow(t, r, id, 100)
	time.Sleep(20 * time.Millisecond)
	if got := sink.lines(); len(got) != 0 {
		t.Fatalf("a partial line was emitted: %q", got)
	}

	appendFile(t, logPath, " line\n")
	waitFor(t, "the completed line", func() bool { return len(sink.lines()) >= 1 })
	if got := sink.lines()[0]; got != "partial line" {
		t.Fatalf("line = %q", got)
	}

	setInstanceState(t, r, id, runtime.StateFailed)
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}
}

func TestFollowLogsTailIsBounded(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	if err := os.WriteFile(logPath, []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink, errc, _ := startFollow(t, r, id, 2)
	waitFor(t, "the bounded tail", func() bool { return len(sink.lines()) >= 2 })

	setInstanceState(t, r, id, runtime.StateSucceeded)
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}
	got := sink.lines()
	if len(got) != 2 || got[0] != "l4" || got[1] != "l5" {
		t.Fatalf("tail = %q, want the last two lines", got)
	}
}

func TestFollowLogsFlushesTheLastPartialLineAtTheEnd(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	// A tool that dies mid-line still has something to say.
	if err := os.WriteFile(logPath, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink, errc, _ := startFollow(t, r, id, 100)
	setInstanceState(t, r, id, runtime.StateFailed)
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}
	if got := sink.lines(); len(got) != 1 || got[0] != "done" {
		t.Fatalf("lines = %q", got)
	}
}

func TestFollowLogsSanitizesCarriageReturns(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	if err := os.WriteFile(logPath, []byte("50%\r100%\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sink, errc, _ := startFollow(t, r, id, 100)
	waitFor(t, "the progress line", func() bool { return len(sink.lines()) >= 1 })
	setInstanceState(t, r, id, runtime.StateSucceeded)
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}

	if got := sink.lines(); len(got) != 1 || got[0] != "100%" {
		t.Fatalf("lines = %q, want the segment a terminal would show", got)
	}
}

func TestFollowLogsEndsWhenTheRecordIsGone(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	if err := os.WriteFile(logPath, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink, errc, _ := startFollow(t, r, id, 100)
	waitFor(t, "the line", func() bool { return len(sink.lines()) >= 1 })

	if err := os.Remove(runtime.InstancePath(r.ConfigDir, id)); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, errc); err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	last := events[len(events)-1]
	if last.Kind != LogEventEnd || last.Note == "" {
		t.Fatalf("last event = %+v, want an end with a note", last)
	}
}

func TestFollowLogsCancelsOnContext(t *testing.T) {
	r, id, logPath := logFixture(t, runtime.StateRunning)
	fastFollow(r)
	if err := os.WriteFile(logPath, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &eventSink{}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.FollowLogs(ctx, "alice", id, 100, sink.emit) }()

	waitFor(t, "the first line", func() bool { return len(sink.lines()) >= 1 })
	cancel()
	if err := waitDone(t, errc); err != nil {
		t.Fatalf("a cancelled stream is a clean end, got %v", err)
	}
}

func TestFollowLogsRejectsUnknownAndAmbiguous(t *testing.T) {
	r, id, _ := logFixture(t, runtime.StateRunning)
	fastFollow(r)

	sink := &eventSink{}
	if err := r.FollowLogs(context.Background(), "alice", "nope", 10, sink.emit); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown instance = %v, want ErrNotFound", err)
	}
	// Another user's instance is not found, not forbidden: the read side does
	// not confirm that it exists.
	if err := r.FollowLogs(context.Background(), "bob", id, 10, sink.emit); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's instance = %v, want ErrNotFound", err)
	}

	// Two instances ending in the same suffix: the caller must disambiguate.
	second := "alice-other-job1"
	if err := runtime.SaveInstance(runtime.InstancePath(r.ConfigDir, second), &runtime.Instance{
		ID: second, User: "alice", Tool: "other", Kind: "task",
		State: runtime.StateRunning, LogPath: filepath.Join(t.TempDir(), "o.log"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.FollowLogs(context.Background(), "alice", "job1", 10, sink.emit); !errors.Is(err, ErrBadRequest) {
		t.Errorf("ambiguous suffix = %v, want ErrBadRequest", err)
	}

	// A terminal instance replays its tail and returns without waiting.
	setInstanceState(t, r, id, runtime.StateSucceeded)
	if err := r.FollowLogs(context.Background(), "alice", id, 10, sink.emit); err != nil {
		t.Errorf("terminal instance = %v, want a clean end", err)
	}
	if got := sink.lines(); len(got) != 0 {
		t.Errorf("lines = %q, want none (the log does not exist)", got)
	}
}
