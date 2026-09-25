package accessrequest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateAndDecide(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	r, created, err := Create(dir, "alice", "scrna_qc", "  要跑一次 QC  ", now)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("first request should be created")
	}
	if r.State != Pending || r.User != "alice" || r.Tool != "scrna_qc" {
		t.Fatalf("request = %+v", r)
	}
	if r.Reason != "要跑一次 QC" {
		t.Fatalf("reason should be trimmed, got %q", r.Reason)
	}
	if !ValidID(r.ID) {
		t.Fatalf("id %q is not filesystem-safe", r.ID)
	}

	// A second request for the same (user, tool) returns the pending one.
	again, created, err := Create(dir, "alice", "scrna_qc", "", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if created || again.ID != r.ID {
		t.Fatalf("a second request must not create a file: created=%v id=%s", created, again.ID)
	}

	decided, err := Decide(dir, r.ID, "root", true, "", now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if decided.State != Approved || decided.DecidedBy != "root" || decided.DecidedAt.IsZero() {
		t.Fatalf("decided = %+v", decided)
	}

	// Deciding again is an error, not a silent overwrite.
	if _, err := Decide(dir, r.ID, "root", false, "changed my mind", now); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second decide = %v, want ErrNotPending", err)
	}

	// After a decision a fresh request is allowed again.
	if _, created, err := Create(dir, "alice", "scrna_qc", "again", now); err != nil || !created {
		t.Fatalf("after a decision a new request should be created: created=%v err=%v", created, err)
	}
}

func TestDenyRecordsNote(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	r, _, err := Create(dir, "bob", "sleeper", "why not", now)
	if err != nil {
		t.Fatal(err)
	}
	denied, err := Decide(dir, r.ID, "root", false, "not for you", now)
	if err != nil {
		t.Fatal(err)
	}
	if denied.State != Denied || denied.Note != "not for you" {
		t.Fatalf("denied = %+v", denied)
	}
}

func TestListAndListFor(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i, spec := range []struct{ user, tool string }{
		{"alice", "a"}, {"bob", "b"}, {"alice", "c"},
	} {
		if _, _, err := Create(dir, spec.user, spec.tool, "", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List = %d, want 3", len(all))
	}
	if !all[0].CreatedAt.Before(all[2].CreatedAt) {
		t.Fatal("List should be oldest first")
	}
	mine, err := ListFor(dir, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 || mine[0].Tool != "a" || mine[1].Tool != "c" {
		t.Fatalf("ListFor(alice) = %+v", mine)
	}
}

func TestGetMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Get(dir, "req-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
	// An id with a path separator must never reach the filesystem.
	if _, err := Get(dir, "../../etc/passwd"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(traversal) = %v, want ErrNotFound", err)
	}
}

func TestConcurrentCreatesDoNotCollide(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	done := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, _, err := Create(dir, "alice", "demo", "concurrent", now)
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	// The same (user, tool) must yield exactly one file even under concurrent
	// clicks: the dedupe scan and the write are serialized.
	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("concurrent creates wrote %d files, want 1", len(all))
	}
	if r := all[0]; r.Tool != "demo" || r.User != "alice" || !ValidID(r.ID) {
		t.Fatalf("torn or wrong request: %+v", r)
	}
}

func TestRequestFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Create(dir, "alice", "demo", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(Dir(dir))
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir: %v (%d)", err, len(entries))
	}
	info, err := os.Stat(filepath.Join(Dir(dir), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("request file mode = %o, want 600", perm)
	}
	if !strings.HasSuffix(entries[0].Name(), ".yaml") {
		t.Fatalf("unexpected file name %s", entries[0].Name())
	}
}
