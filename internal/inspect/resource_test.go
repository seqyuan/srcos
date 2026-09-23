package inspect

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/resource"
	"github.com/seqyuan/srcos/internal/storage"
)

// seedResourceFixture puts files in alice's home, in the demo tool's workspace,
// and in the two storages, so every scope has something to resolve.
func seedResourceFixture(t *testing.T, f *fixture) {
	t.Helper()
	mustWrite := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home := config.HomeDir(f.configDir, "alice")
	mustWrite(filepath.Join(home, "notes.md"), "# hello\n\nworld\n")
	mustWrite(filepath.Join(home, "data.csv"), "a,b\n1,2\n")
	mustWrite(filepath.Join(home, "secret.bin"), "x\x00y")
	mustWrite(filepath.Join(home, "sub", "deep.txt"), "deep\n")

	ws := config.WorkspaceDir(f.configDir, "alice", "demo")
	mustWrite(filepath.Join(ws, "out", "counts.txt"), "1\n2\n")

	mustWrite(filepath.Join(f.dataRoot, "ref", "genes.tsv"), "gene\tvalue\nA\t1\n")
	mustWrite(filepath.Join(f.scratch, "tmp.log"), "log line\n")
}

func TestResolveResourceHome(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	view, err := f.ViewResource("alice", ResourceRequest{Scope: "home", Path: "notes.md"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.IsDir {
		t.Error("notes.md must not be a directory")
	}
	if view.SandboxPath != "/home/alice/notes.md" {
		t.Errorf("sandbox path = %q", view.SandboxPath)
	}
	if view.Mode != "rw" {
		t.Errorf("mode = %q, want rw", view.Mode)
	}
	if view.Viewer.Kind != resource.KindMarkdown {
		t.Errorf("viewer = %+v, want markdown", view.Viewer)
	}
	if !strings.Contains(view.Text, "# hello") {
		t.Errorf("text payload = %q", view.Text)
	}
	// An address is user-relative: bob asking for the same address gets bob's
	// home, which is a different (here: empty) place.
	if _, err := f.ViewResource("bob", ResourceRequest{Scope: "home", Path: "notes.md"}, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob must not see alice's home: %v", err)
	}
}

func TestResolveResourceWorkspace(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	view, err := f.ViewResource("alice", ResourceRequest{Scope: "workspace", Path: "out/counts.txt", Tool: "demo"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.SandboxPath != "/workspace/out/counts.txt" {
		t.Errorf("sandbox path = %q", view.SandboxPath)
	}

	// The tool is the disambiguator, so it is required.
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "workspace", Path: "out/counts.txt"}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("missing tool = %v, want ErrBadRequest", err)
	}
	// A tool the user is not granted is hidden, not refused (same class as
	// unknown: telling a caller a tool exists but is not theirs is a leak).
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "workspace", Path: "x", Tool: "other"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ungranted tool = %v, want ErrNotFound", err)
	}
}

func TestResolveResourceStorageClosure(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo")) // demo requires only "data"
	seedResourceFixture(t, f)

	view, err := f.ViewResource("alice", ResourceRequest{Scope: "data", Path: "ref/genes.tsv"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.Mode != "ro" {
		t.Errorf("mode = %q, want ro", view.Mode)
	}
	if view.Viewer.Kind != resource.KindTable {
		t.Errorf("viewer = %+v, want table", view.Viewer)
	}

	// "scratch" exists but no tool alice can see declares it: that is a
	// disclosure-shaped no, not a "not found".
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "scratch", Path: "tmp.log"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("undeclared storage = %v, want ErrForbidden", err)
	}
	// A storage that is not declared at all is simply unknown.
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "nope", Path: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown storage = %v, want ErrNotFound", err)
	}
}

func TestResolveResourceRefusesEscapes(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	cases := []struct {
		name string
		req  ResourceRequest
	}{
		{"dotdot", ResourceRequest{Scope: "home", Path: "../../etc/passwd"}},
		{"deep dotdot", ResourceRequest{Scope: "home", Path: "sub/../../../etc/passwd"}},
		{"storage dotdot", ResourceRequest{Scope: "data", Path: "../home/alice/notes.md"}},
	}
	for _, c := range cases {
		if _, err := f.ResolveResource("alice", c.req); err == nil {
			t.Errorf("%s: escape was allowed", c.name)
		} else if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: error = %v, want ErrForbidden", c.name, err)
		}
	}

	// A symlink planted in a writable scope must not redirect the lookup.
	link := filepath.Join(config.HomeDir(f.configDir, "alice"), "escape")
	if err := os.Symlink("/etc/passwd", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "home", Path: "escape"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("symlink escape = %v, want ErrForbidden", err)
	}
}

func TestResolveResourceDirectoryAndListing(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	view, err := f.DescribeResource("alice", ResourceRequest{Scope: "home", Path: ""}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !view.IsDir || view.Viewer.Kind != resource.KindDir {
		t.Fatalf("home root = %+v", view)
	}
	names := map[string]bool{}
	for _, e := range view.Entries {
		names[e.Name] = true
		if e.Rel == "" {
			t.Errorf("entry %q has an empty relative path", e.Name)
		}
	}
	for _, want := range []string{"notes.md", "data.csv", "sub"} {
		if !names[want] {
			t.Errorf("listing is missing %q: %+v", want, names)
		}
	}

	// A directory cannot be streamed as bytes.
	if _, err := f.OpenResource("alice", ResourceRequest{Scope: "home", Path: "sub"}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("opening a directory = %v, want ErrBadRequest", err)
	}
}

func TestViewerOverrideAndBinaryDetection(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	// An explicit viewer overrides the registry's claim.
	view, err := f.ViewResource("alice", ResourceRequest{Scope: "home", Path: "notes.md"}, "text")
	if err != nil {
		t.Fatal(err)
	}
	if view.Viewer.ID != "text" {
		t.Errorf("viewer = %q, want text", view.Viewer.ID)
	}
	if _, err := f.ViewResource("alice", ResourceRequest{Scope: "home", Path: "notes.md"}, "nope"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("unknown viewer = %v, want ErrBadRequest", err)
	}
	// The filesystem wins: a directory stays a directory whatever was asked.
	view, err = f.ViewResource("alice", ResourceRequest{Scope: "home", Path: "sub"}, "text")
	if err != nil {
		t.Fatal(err)
	}
	if view.Viewer.Kind != resource.KindDir {
		t.Errorf("directory override = %+v, want dir", view.Viewer)
	}

	// Binary content is flagged rather than rendered as mojibake.
	view, err = f.ViewResource("alice", ResourceRequest{Scope: "home", Path: "secret.bin"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Binary || view.Text != "" {
		t.Errorf("binary file = %+v, want Binary with no text", view)
	}
}

func TestResourceScopesFollowGrants(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	scopes, err := f.ResourceScopes("alice")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]ResourceScope{}
	for _, s := range scopes {
		byKey[s.Kind+":"+s.Scope] = s
	}
	if _, ok := byKey["home:home"]; !ok {
		t.Error("the home scope is missing")
	}
	if _, ok := byKey["workspace:workspace"]; !ok {
		t.Error("the workspace scope is missing")
	}
	if s, ok := byKey["storage:data"]; !ok || s.Name == "" {
		t.Errorf("the data storage scope is missing: %+v", byKey)
	}
	if _, ok := byKey["storage:scratch"]; ok {
		t.Error("scratch is not declared by any visible tool and must not be offered")
	}
}

// A deployment with no storage provider must still answer for home/workspace,
// and must say so plainly for a storage scope.
func TestResolveResourceWithoutStorageProvider(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)
	f.Storages = nil
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "data", Path: "ref"}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("storage scope without a provider = %v, want ErrUnavailable", err)
	}
	if _, err := f.ResolveResource("alice", ResourceRequest{Scope: "home", Path: ""}); err != nil {
		t.Errorf("home scope must work without a provider: %v", err)
	}
}

// Storage ids home/workspace are refused at registration: the scope names are
// reserved, and a storage called "home" would make an address ambiguous.
func TestReservedStorageIDs(t *testing.T) {
	for _, id := range []string{"home", "workspace"} {
		_, err := storage.New([]storage.Storage{{
			ID: id, Name: id, Kind: storage.KindPosix,
			HostRoot: "/tmp", SandboxPath: "/" + id, Mode: storage.ReadOnly,
		}})
		if err == nil {
			t.Errorf("storage id %q was accepted", id)
		}
	}
}
