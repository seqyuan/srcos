package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordAndQueryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	defer r.Close()

	r.Record(NewEvent(Actor{User: "alice", Kind: KindSession}, "submit").
		WithTarget("tool", "demo", "0.1.0").
		WithParams(map[string]any{"samples": "S1,S2", "cpu": 4}).
		WithRefs(map[string]string{"job": "j1", "instance": "i1"}).
		Allowed())
	r.Record(NewEvent(Actor{User: "bob", Kind: KindAgentToken, TokenID: "abc"}, "submit").
		WithTarget("tool", "sleeper", "").
		Denied("credential may not submit to tool sleeper"))

	all, err := Query(dir, Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d events, want 2", len(all))
	}
	first := all[0]
	if first.Actor.User != "alice" || first.Action != "submit" || first.Decision != Allow {
		t.Fatalf("unexpected first event: %+v", first)
	}
	if first.Target.ID != "demo" || first.Target.Version != "0.1.0" {
		t.Fatalf("target lost: %+v", first.Target)
	}
	if first.Params["samples"] != "S1,S2" || first.Params["cpu"] != "4" {
		t.Fatalf("params lost: %+v", first.Params)
	}
	if first.Refs["job"] != "j1" || first.Refs["instance"] != "i1" {
		t.Fatalf("refs lost: %+v", first.Refs)
	}
	if !first.Outcome.OK {
		t.Fatalf("allowed event should carry ok outcome: %+v", first.Outcome)
	}
	second := all[1]
	if second.Decision != Deny || second.Reason == "" || second.Outcome != nil {
		t.Fatalf("denied event malformed: %+v", second)
	}
}

func TestFilterAndTail(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	defer r.Close()
	for i := 0; i < 5; i++ {
		r.Record(NewEvent(Actor{User: "alice", Kind: KindSession}, "submit").Allowed())
	}
	r.Record(NewEvent(Actor{User: "bob", Kind: KindCLI}, "submit").Denied("nope"))

	alice, err := Query(dir, Filter{User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 5 {
		t.Fatalf("alice events = %d, want 5", len(alice))
	}
	denied, err := Query(dir, Filter{Decision: Deny})
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 1 || denied[0].Actor.User != "bob" {
		t.Fatalf("denied filter wrong: %+v", denied)
	}
	tail, err := Tail(dir, Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[1].Actor.User != "bob" {
		t.Fatalf("tail should end with the newest: %+v", tail)
	}

	since, err := Query(dir, Filter{Since: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 0 {
		t.Fatalf("future cutoff should match nothing, got %d", len(since))
	}
}

func TestRedactMasksSecrets(t *testing.T) {
	got := Redact(map[string]any{
		"sample":       "S1",
		"db_password":  "hunter2",
		"API_KEY":      "xyz",
		"authToken":    "abc",
		"private_note": "shh",
		"plain":        42,
	})
	for _, k := range []string{"db_password", "API_KEY", "authToken", "private_note"} {
		if got[k] != "***" {
			t.Errorf("%s = %q, want masked", k, got[k])
		}
	}
	if got["sample"] != "S1" || got["plain"] != "42" {
		t.Errorf("non-sensitive values mangled: %+v", got)
	}
	if Redact(nil) != nil {
		t.Error("empty params should stay nil")
	}
}

func TestNilRecorderIsNoOp(t *testing.T) {
	var r *Recorder
	r.Record(NewEvent(Actor{User: "x"}, "submit").Allowed()) // must not panic
	if err := r.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

func TestRotationByDay(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	defer r.Close()

	day1 := time.Date(2026, 9, 25, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC)
	e1 := NewEvent(Actor{User: "alice"}, "submit").Allowed()
	e1.TS = day1
	r.Record(e1)
	e2 := NewEvent(Actor{User: "alice"}, "submit").Allowed()
	e2.TS = day2
	r.Record(e2)

	entries, err := os.ReadDir(filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected two daily files, got %d", len(entries))
	}
	all, err := Query(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("events across days = %d, want 2", len(all))
	}
}

func TestQueryMissingDirectoryIsEmpty(t *testing.T) {
	all, err := Query(t.TempDir(), Filter{})
	if err != nil {
		t.Fatalf("missing audit dir should not error: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("got %d events from an empty dir", len(all))
	}
}

func TestCorruptLineIsSkipped(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	r.Close()

	path := filepath.Join(dir, "audit", "audit-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	all, err := Query(dir, Filter{})
	if err != nil {
		t.Fatalf("a corrupt line must not abort the read: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected the one good event, got %d", len(all))
	}
}

func TestRecordIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	defer r.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				r.Record(NewEvent(Actor{User: "u"}, "submit").Allowed())
			}
		}()
	}
	wg.Wait()

	all, err := Query(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 200 {
		t.Fatalf("lost events under concurrency: got %d, want 200", len(all))
	}
	// Every line must be complete JSON: a torn write would have been skipped.
	for _, e := range all {
		if e.Action == "" || e.Decision == "" {
			t.Fatalf("torn event: %+v", e)
		}
	}
}

func TestFilePermissionsArePrivate(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	r.Close()

	files, err := listFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("listFiles: %v (%d files)", err, len(files))
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("audit file mode = %o, want 600", perm)
	}
	if !strings.Contains(files[0], "audit-") {
		t.Fatalf("unexpected file name %s", files[0])
	}
}

func TestPruneRemovesOnlyOldFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	oldEvent := NewEvent(Actor{User: "alice"}, "submit").Allowed()
	oldEvent.TS = time.Now().UTC().AddDate(0, 0, -100)
	r.Record(oldEvent)
	recentEvent := NewEvent(Actor{User: "alice"}, "submit").Allowed()
	recentEvent.TS = time.Now().UTC()
	r.Record(recentEvent)
	r.Close()

	now := time.Now().UTC()
	keep := 30 * 24 * time.Hour
	stale, err := Stale(dir, keep, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("stale = %v, want exactly the 100-day-old file", stale)
	}

	// Keep == 0 must never remove anything.
	if removed, err := Prune(dir, 0, now); err != nil || len(removed) != 0 {
		t.Fatalf("keep=0 removed %v (%v)", removed, err)
	}

	removed, err := Prune(dir, keep, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want 1", removed)
	}
	events, err := Query(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("after prune, events = %d, want only the recent one", len(events))
	}
}

func TestParseKeep(t *testing.T) {
	cases := map[string]time.Duration{
		"90d": 90 * 24 * time.Hour,
		"48h": 48 * time.Hour,
		"0":   0,
		"":    0,
	}
	for in, want := range cases {
		got, err := ParseKeep(in)
		if err != nil {
			t.Fatalf("ParseKeep(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseKeep(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseKeep("nonsense"); err == nil {
		t.Error("ParseKeep(nonsense) should fail")
	}
}

func TestHashChainVerifiesAcrossRecorderRestarts(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	r.Record(NewEvent(Actor{User: "alice"}, "cancel").Allowed())
	r.Close()

	// A second recorder (a restarted process) must continue the chain, not
	// start a fresh one.
	r2 := New(dir)
	r2.Record(NewEvent(Actor{User: "bob"}, "submit").Allowed())
	r2.Close()

	problems, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("an untouched chain must verify, got %+v", problems)
	}
}

func TestVerifyDetectsAlteredLine(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	r.Close()

	files, err := listFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("listFiles: %v (%d)", err, len(files))
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	// Same length, different value: the line no longer hashes to its own Hash.
	altered := strings.Replace(string(data), `"action":"submit"`, `"action":"cancll"`, 1)
	if err := os.WriteFile(files[0], []byte(altered), 0o600); err != nil {
		t.Fatal(err)
	}

	problems, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("an altered line must be detected")
	}
}

func TestVerifyDetectsDeletedLine(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)
	for i := 0; i < 3; i++ {
		r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	}
	r.Close()

	files, err := listFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("listFiles: %v (%d)", err, len(files))
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	// Drop the middle line: the third line's Prev no longer matches.
	if err := os.WriteFile(files[0], []byte(lines[0]+"\n"+lines[2]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	problems, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("a deleted line must break the chain and be detected")
	}
}

func TestVerifyEmptyStreamIsFine(t *testing.T) {
	problems, err := Verify(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("empty stream should verify, got %+v", problems)
	}
}

// Two recorders appending to the same file is what the gateway and a CLI
// invocation look like. Each must chain onto what the file actually ends with,
// not onto its own memory — otherwise interleaved writers break the chain
// (caught by make e2e before the lock was added).
func TestChainSurvivesInterleavedRecorders(t *testing.T) {
	dir := t.TempDir()
	a, b := New(dir), New(dir)
	for i := 0; i < 10; i++ {
		a.Record(NewEvent(Actor{User: "gateway"}, "submit").Allowed())
		b.Record(NewEvent(Actor{User: "cli"}, "token.create").Allowed())
	}
	problems, err := Verify(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("interleaved writers broke the chain: %+v", problems)
	}
	if events, err := Query(dir, Filter{}); err != nil || len(events) != 20 {
		t.Fatalf("events = %d (%v), want 20", len(events), err)
	}
}
