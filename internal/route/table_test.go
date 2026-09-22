package route

import (
	"strings"
	"testing"
)

func sample(user, tool, id string, port int) Entry {
	return Entry{
		User:       user,
		Tool:       tool,
		InstanceID: id,
		Target:     Target{Host: "127.0.0.1", Port: port},
		WebSocket:  true,
	}
}

func TestPutAndGet(t *testing.T) {
	tab := NewTable()
	if err := tab.Put(sample("alice", "jupyter", "i1", 20001)); err != nil {
		t.Fatal(err)
	}
	e, ok := tab.Get("alice", "jupyter")
	if !ok {
		t.Fatal("route not found")
	}
	if e.Target.String() != "127.0.0.1:20001" {
		t.Fatalf("target = %s", e.Target)
	}
	// The path is derived, never taken from user input.
	if e.Path != "/proxy/alice/jupyter" {
		t.Fatalf("path = %q", e.Path)
	}
}

func TestPutValidatesTarget(t *testing.T) {
	tab := NewTable()
	// A non-loopback target would make the table an SSRF surface.
	e := sample("alice", "x", "i1", 80)
	e.Target.Host = "example.com"
	if err := tab.Put(e); err == nil {
		t.Fatal("a non-loopback target must be refused")
	}
	e = sample("alice", "x", "i1", 0)
	if err := tab.Put(e); err == nil {
		t.Fatal("port 0 must be refused")
	}
	e = sample("alice", "x", "i1", 70000)
	if err := tab.Put(e); err == nil {
		t.Fatal("an out-of-range port must be refused")
	}
	e = sample("", "x", "i1", 20001)
	if err := tab.Put(e); err == nil {
		t.Fatal("a missing user must be refused")
	}
	e = sample("alice", "x", "", 20001)
	if err := tab.Put(e); err == nil {
		t.Fatal("a missing instance id must be refused")
	}
	e = sample("alice", "x", "i1", 20001)
	e.Path = "relative"
	if err := tab.Put(e); err == nil {
		t.Fatal("a relative path must be refused")
	}
}

func TestDefaultTargetIsLoopback(t *testing.T) {
	tab := NewTable()
	e := sample("alice", "x", "i1", 20001)
	e.Target.Host = ""
	if err := tab.Put(e); err != nil {
		t.Fatal(err)
	}
	got, _ := tab.Get("alice", "x")
	if got.Target.Host != "127.0.0.1" {
		t.Fatalf("host = %q", got.Target.Host)
	}
}

// TestPutReplaces covers "one live instance per (user, tool)": starting a new
// instance supersedes the previous route rather than accumulating entries.
func TestPutReplaces(t *testing.T) {
	tab := NewTable()
	if err := tab.Put(sample("alice", "jupyter", "i1", 20001)); err != nil {
		t.Fatal(err)
	}
	if err := tab.Put(sample("alice", "jupyter", "i2", 20002)); err != nil {
		t.Fatal(err)
	}
	if tab.Len() != 1 {
		t.Fatalf("len = %d", tab.Len())
	}
	e, _ := tab.Get("alice", "jupyter")
	if e.InstanceID != "i2" || e.Target.Port != 20002 {
		t.Fatalf("stale route: %+v", e)
	}
}

func TestGetByPathMatchesOnSegmentBoundary(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("alice", "jupyter", "i1", 20001))

	cases := map[string]bool{
		"/proxy/alice/jupyter":          true,
		"/proxy/alice/jupyter/":         true,
		"/proxy/alice/jupyter/lab/tree": true,
		"/proxy/alice/jupyterx":         false, // prefix but not a segment
		"/proxy/alice":                  false,
		"/proxy/bob/jupyter":            false,
		"/proxy/alice/jupyter-labs":     false,
	}
	for path, want := range cases {
		_, _, ok := tab.GetByPath(path)
		if ok != want {
			t.Errorf("GetByPath(%q) = %v, want %v", path, ok, want)
		}
	}
}

func TestGetByPathReturnsRemainder(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("alice", "jupyter", "i1", 20001))

	// The proxy needs the path *after* the prefix to rewrite the request.
	_, rest, ok := tab.GetByPath("/proxy/alice/jupyter/lab/tree")
	if !ok {
		t.Fatal("not found")
	}
	if rest != "/lab/tree" {
		t.Fatalf("rest = %q", rest)
	}
	_, rest, _ = tab.GetByPath("/proxy/alice/jupyter")
	if rest != "" {
		t.Fatalf("rest for the exact prefix = %q", rest)
	}
}

func TestGetByPathPrefersLongestPrefix(t *testing.T) {
	// Tool ids may be prefixes of one another; the longer path must win.
	tab := NewTable()
	_ = tab.Put(sample("alice", "r", "i1", 20001))
	_ = tab.Put(sample("alice", "rstudio", "i2", 20002))

	e, _, ok := tab.GetByPath("/proxy/alice/rstudio/s/1")
	if !ok || e.Tool != "rstudio" {
		t.Fatalf("got %+v", e)
	}
	e, _, ok = tab.GetByPath("/proxy/alice/r/x")
	if !ok || e.Tool != "r" {
		t.Fatalf("got %+v", e)
	}
}

func TestDelete(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("alice", "jupyter", "i1", 20001))
	if !tab.Delete("alice", "jupyter") {
		t.Fatal("delete should report removal")
	}
	if tab.Delete("alice", "jupyter") {
		t.Fatal("a second delete should report nothing removed")
	}
}

// TestDeleteInstanceIsFenced covers the reaper's guard: it must not tear down a
// successor's route when cleaning up a stale instance.
func TestDeleteInstanceIsFenced(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("alice", "jupyter", "new", 20002))

	if tab.DeleteInstance("alice", "jupyter", "old") {
		t.Fatal("the reaper removed a successor's route")
	}
	if tab.Len() != 1 {
		t.Fatal("route was removed")
	}
	if !tab.DeleteInstance("alice", "jupyter", "new") {
		t.Fatal("the owning instance should be able to remove its route")
	}
}

func TestListIsStable(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("bob", "rstudio", "i3", 20003))
	_ = tab.Put(sample("alice", "jupyter", "i1", 20001))
	_ = tab.Put(sample("alice", "r", "i2", 20002))

	list := tab.List()
	got := []string{list[0].Path, list[1].Path, list[2].Path}
	want := []string{"/proxy/alice/jupyter", "/proxy/alice/r", "/proxy/bob/rstudio"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v", got)
	}
}

func TestForUser(t *testing.T) {
	tab := NewTable()
	_ = tab.Put(sample("alice", "jupyter", "i1", 20001))
	_ = tab.Put(sample("bob", "rstudio", "i2", 20002))
	if got := tab.ForUser("alice"); len(got) != 1 || got[0].Tool != "jupyter" {
		t.Fatalf("ForUser = %+v", got)
	}
}

func TestDefaultPathIsSafe(t *testing.T) {
	// Usernames and tool ids are validated upstream, so the composed path can
	// never contain a separator or "..".
	got := DefaultPath("alice", "jupyter-lab")
	if got != "/proxy/alice/jupyter-lab" {
		t.Fatalf("DefaultPath = %q", got)
	}
	if strings.Contains(got, "..") {
		t.Fatal("path traversal in a derived path")
	}
}

func TestConcurrentPutGetDelete(t *testing.T) {
	tab := NewTable()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = tab.Put(sample("alice", "jupyter", "i1", 20001+i%10))
			tab.GetByPath("/proxy/alice/jupyter/lab")
			tab.List()
			tab.Delete("alice", "jupyter")
		}
	}()
	for i := 0; i < 200; i++ {
		_, _, _ = tab.GetByPath("/proxy/alice/jupyter")
		tab.Len()
	}
	<-done
}
