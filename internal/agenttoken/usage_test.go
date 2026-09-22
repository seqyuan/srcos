package agenttoken

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRecordsLastUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "agent-token-usage.yaml")
	u := LoadUsage(path)

	// Nothing touched: no file is created (an idle gateway stays idle).
	if err := u.Flush(); err != nil {
		t.Fatalf("Flush on a clean store: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Flush wrote a file with nothing to record: %v", err)
	}

	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	// The first use of a token is written immediately: that is the moment an
	// operator wants `token list` to show it.
	if err := u.Touch("abcdefgh", first); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if got := u.Last("abcdefgh"); !got.Equal(first) {
		t.Fatalf("Last = %v, want %v", got, first)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the first use should have written %s: %v", path, err)
	}

	// It survives a reload, which is what `srcos token list` relies on.
	again := LoadUsage(path)
	if got := again.Last("abcdefgh"); !got.Equal(first) {
		t.Fatalf("reloaded Last = %v, want %v", got, first)
	}

	// A second use moves it forward; an out-of-order (clock-skewed) use does
	// not move it backwards. Both are throttled now, so flush explicitly.
	later := first.Add(time.Hour)
	if err := u.Touch("abcdefgh", later); err != nil {
		t.Fatal(err)
	}
	if err := u.Touch("abcdefgh", first.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := u.Last("abcdefgh"); !got.Equal(later) {
		t.Fatalf("Last = %v, want %v (a skewed timestamp must not go backwards)", got, later)
	}
	if err := u.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := LoadUsage(path).Last("abcdefgh"); !got.Equal(later) {
		t.Fatalf("persisted Last = %v, want %v", got, later)
	}
}

// Usage is a hint, never a reason to fail: damaged files and missing files
// both mean "no known usage".
func TestUsageToleratesDamage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-token-usage.yaml")

	if got := LoadUsage(path); !got.Last("abcdefgh").IsZero() {
		t.Fatal("a missing file means no known usage")
	}
	if err := os.WriteFile(path, []byte("usage: {this is: [not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadUsage(path); !got.Last("abcdefgh").IsZero() {
		t.Fatal("a malformed file means no known usage")
	}
}

// A nil *Usage is legal: the CLI (and any caller that does not maintain usage)
// must not have to check.
func TestNilUsageIsInert(t *testing.T) {
	var u *Usage
	if err := u.Touch("abcdefgh", time.Now()); err != nil {
		t.Fatalf("Touch on nil: %v", err)
	}
	if err := u.Flush(); err != nil {
		t.Fatalf("Flush on nil: %v", err)
	}
	if !u.Last("abcdefgh").IsZero() {
		t.Fatal("Last on nil must be the zero time")
	}
}

// A token use must never be able to fail because usage bookkeeping broke.
func TestAuthenticationSucceedsEvenIfUsageCannotBeWritten(t *testing.T) {
	s, _ := newTestStore(t)
	_, raw := mustCreate(t, s, CreateParams{User: "alice"})

	// A regular file where a directory would have to be: every write fails.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.AttachUsage(LoadUsage(filepath.Join(blocker, "agent-token-usage.yaml")))

	if err := s.usage.Touch("abcdefgh", time.Now()); err == nil {
		t.Fatal("the fixture must actually make the usage write fail")
	}
	if _, err := s.verifyAt(raw, time.Now()); err != nil {
		t.Fatalf("a usage write failure must not reject a valid token: %v", err)
	}
}
