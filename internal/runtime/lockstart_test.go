package runtime

import (
	"testing"
	"time"
)

// LockStart must serialize the same (user, tool) across runners that share the
// table (ForUser clones), and must not block a different pair. This is what
// keeps two concurrent service starts from stopping each other's in-flight
// instance (#4).
func TestLockStartSerializesSharedPairs(t *testing.T) {
	base := NewRunner(Options{})
	user := base.ForUser("alice")

	unlock := user.LockStart("alice", "web")

	acquired := make(chan struct{})
	go func() {
		unlock2 := base.LockStart("alice", "web") // same pair, shared table
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("the second LockStart for the same (user, tool) must wait while the first is held")
	case <-time.After(50 * time.Millisecond):
	}

	unlock()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the second LockStart never acquired after the first released")
	}

	// A different pair is independent: it must not block on the same table.
	other := make(chan struct{})
	go func() {
		u := base.LockStart("alice", "jupyter")
		close(other)
		u()
	}()
	select {
	case <-other:
	case <-time.After(2 * time.Second):
		t.Fatal("a different (user, tool) must not block")
	}
}

// A nil Runner (a deployment with no supervisor) must yield a no-op unlock, not
// a panic: the callers are on the request path.
func TestLockStartNilRunnerIsNoop(t *testing.T) {
	var r *Runner
	unlock := r.LockStart("alice", "web")
	unlock()
}
