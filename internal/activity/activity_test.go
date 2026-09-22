package activity

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTouchAndLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "activity.yaml")
	j := Load(path, "# test\n\n")

	// Nothing touched: no file is created (an idle deployment stays idle).
	if err := j.Flush(); err != nil {
		t.Fatalf("Flush on a clean journal: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Flush wrote a file with nothing to record: %v", err)
	}

	first := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	// The first use of an id is written immediately: that is the moment a
	// reader cares.
	if err := j.Touch("alpha", first); err != nil {
		t.Fatal(err)
	}
	if got := j.Last("alpha"); !got.Equal(first) {
		t.Fatalf("Last = %v, want %v", got, first)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the first touch should have written %s: %v", path, err)
	}
	if got := Load(path, "").Last("alpha"); !got.Equal(first) {
		t.Fatalf("persisted = %v, want %v", got, first)
	}

	// A later use moves it forward; a clock-skewed one does not move it back.
	later := first.Add(time.Hour)
	if err := j.Touch("alpha", later); err != nil {
		t.Fatal(err)
	}
	if err := j.Touch("alpha", first.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := j.Last("alpha"); !got.Equal(later) {
		t.Fatalf("Last = %v, want %v (a skewed timestamp must not go backwards)", got, later)
	}
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "").Last("alpha"); !got.Equal(later) {
		t.Fatalf("persisted = %v, want %v", got, later)
	}

	// Ids are independent.
	if !j.Last("beta").IsZero() {
		t.Fatal("an untouched id must be unknown")
	}
}

// Damage means "no recorded activity": it must never be a reason to refuse a
// request, and never a reason to reap something.
func TestDamageIsTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.yaml")
	if got := Load(path, "").Last("alpha"); !got.IsZero() {
		t.Fatal("a missing file means no known activity")
	}
	if err := os.WriteFile(path, []byte("entries: {this is: [not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Load(path, "").Last("alpha"); !got.IsZero() {
		t.Fatal("a malformed file means no known activity")
	}
}

// A journal whose file cannot be written must still accept touches: the hint is
// never more important than the work.
func TestWriteFailureIsReportedNotFatal(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	j := Load(filepath.Join(blocker, "activity.yaml"), "")
	if err := j.Touch("alpha", time.Now()); err == nil {
		t.Fatal("a write failure must be reported")
	}
	if j.Last("alpha").IsZero() {
		t.Fatal("the in-memory value must survive the failed write")
	}
}

// A nil journal is legal, so a caller that does not track activity need not
// check.
func TestNilJournalIsInert(t *testing.T) {
	var j *Journal
	if err := j.Touch("alpha", time.Now()); err != nil {
		t.Fatalf("Touch on nil: %v", err)
	}
	if err := j.Flush(); err != nil {
		t.Fatalf("Flush on nil: %v", err)
	}
	if !j.Last("alpha").IsZero() {
		t.Fatal("Last on nil must be the zero time")
	}
	if err := j.Touch("", time.Now()); err != nil {
		t.Fatalf("an empty id is not an error: %v", err)
	}
}

func TestHeaderIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.yaml")
	j := Load(path, "# keep me\n\n")
	if err := j.Touch("alpha", time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 10 || string(data[:10]) != "# keep me\n" {
		t.Fatalf("header lost: %q", data[:20])
	}
}
