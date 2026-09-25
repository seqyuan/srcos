package inspect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// fixture is a whole little deployment: two users, two tools, two storages.
type fixture struct {
	*Reader
	configDir string
	toolsDir  string
	dataRoot  string // storage "data"  → /data
	scratch   string // storage "scratch" → /scratch
}

const taskManifest = `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: bwrap
entry: work.sh
interface:
  inputs:
    - {name: ref, type: path, from: data, select: directory, required: true}
    - {name: note, type: string, default: hi}
  outputs:
    - {name: out, type: directory}
resources: {cpu: 1, memory: "1Gi", walltime: "0:10:00"}
requires_storages: [data]
`

const serviceManifest = `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080}
lifecycle: {restart: never, max_lifetime: "1h"}
`

const otherManifest = `
schemaVersion: 1
id: other
version: 0.1.0
name: Other
kind: task
backend: local
sandbox: bwrap
entry: work.sh
interface:
  inputs:
    - {name: scratch, type: path, from: scratch, select: directory}
resources: {cpu: 1, memory: "1Gi", walltime: "0:10:00"}
requires_storages: [scratch]
`

func newFixture(t *testing.T, grants Grants) *fixture {
	t.Helper()
	configDir := t.TempDir()
	for _, user := range []string{"alice", "bob"} {
		path := config.UserConfigPath(configDir, user)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("auth:\n  password_hash: \""+strings.Repeat("1", 64)+"\"\nservices: []\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	toolsDir := t.TempDir()
	for id, manifest := range map[string]string{"demo": taskManifest, "web": serviceManifest, "other": otherManifest} {
		dir := filepath.Join(toolsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "ref", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "readme.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	provider, err := storage.New([]storage.Storage{
		{ID: "data", Name: "Data", Kind: storage.KindPosix, HostRoot: dataRoot, SandboxPath: "/data", Mode: storage.ReadOnly},
		{ID: "scratch", Name: "Scratch", Kind: storage.KindPosix, HostRoot: scratch, SandboxPath: "/scratch", Mode: storage.ReadWrite},
	})
	if err != nil {
		t.Fatal(err)
	}

	return &fixture{
		Reader: &Reader{
			ConfigDir: configDir,
			ToolsDir:  toolsDir,
			Storages:  provider,
			Grants:    grants,
		},
		configDir: configDir,
		toolsDir:  toolsDir,
		dataRoot:  dataRoot,
		scratch:   scratch,
	}
}

// allowOnly builds a policy that grants one tool publicly.
func allowOnly(t *testing.T, toolID string) *grant.Policy {
	t.Helper()
	p, err := grant.New(nil, nil, []grant.Grant{{Tool: toolID, Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVisibilityFollowsGrants(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))

	views, err := f.Tools("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != "demo" {
		t.Fatalf("catalogue = %+v", views)
	}

	if _, err := f.Tool("alice", "demo"); err != nil {
		t.Fatalf("a granted tool must be described: %v", err)
	}

	// A tool nobody is granted is invisible — and so is one that exists but is
	// not granted to *this* user: the two must be indistinguishable.
	if _, err := f.Tool("alice", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted tool: %v", err)
	}
	if _, err := f.Tool("alice", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tool: %v", err)
	}
	if _, err := f.Tool("alice", "wen"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no tool should be reachable by a typo nearby: %v", err)
	}

	// A manifest lookup without the grant check is what instance-scoped reads
	// use: the tool exists even when it is no longer granted.
	if _, err := f.Manifest("web"); err != nil {
		t.Fatalf("Manifest must not consult grants: %v", err)
	}
}

// storagesForUser is the union over visible tools: a storage only a tool this
// user cannot see declares must not appear.
func TestStoragesForUserFollowsVisibility(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))

	views, err := f.StoragesForUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != "data" {
		t.Fatalf("storages = %+v", views)
	}
	if len(views[0].Tools) != 1 || views[0].Tools[0] != "demo" {
		t.Fatalf("a storage must name the tools that declare it: %+v", views[0])
	}

	// With everything granted both storages appear, and the host roots stay out
	// of the answer.
	all := newFixture(t, nil)
	views, err = all.StoragesForUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 {
		t.Fatalf("expected both storages, got %+v", views)
	}
	for _, v := range views {
		if v.Root != "/data" && v.Root != "/scratch" {
			t.Fatalf("a storage view must carry the sandbox root, got %q", v.Root)
		}
		if strings.Contains(v.Root, all.configDir) {
			t.Fatal("a host path leaked into a storage view")
		}
	}
}

func TestPathsEnforcesTheClosure(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	ctx := context.Background()

	listing, err := f.Paths(ctx, "alice", PathRequest{Tool: "demo", Input: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	if listing.Storage != "data" || listing.Path != "/data" || listing.Select != "directory" {
		t.Fatalf("listing = %+v", listing)
	}
	if len(listing.Entries) != 1 || listing.Entries[0].Path != "/data/ref" {
		t.Fatalf("entries = %+v", listing.Entries)
	}

	// A host with no storage provider cannot answer at all, which is a
	// different answer from "you may not have this".
	bare := &Reader{ConfigDir: f.configDir, ToolsDir: f.toolsDir}
	if _, err := bare.Paths(ctx, "alice", PathRequest{Tool: "demo", Input: "ref"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no provider: %v", err)
	}

	cases := []struct {
		name string
		req  PathRequest
		want error
	}{
		{"missing tool", PathRequest{Tool: "demo"}, ErrBadRequest},
		{"unknown input", PathRequest{Tool: "demo", Input: "nope"}, ErrNotFound},
		{"ungranted tool", PathRequest{Tool: "other", Input: "scratch"}, ErrNotFound},
		{"not storage-bound", PathRequest{Tool: "demo", Input: "note"}, ErrBadRequest},
		{"undeclared storage", PathRequest{Tool: "demo", Input: "ref", Storage: "scratch"}, ErrForbidden},
		{"escape", PathRequest{Tool: "demo", Input: "ref", Path: "/data/../.."}, ErrBadRequest},
		{"outside", PathRequest{Tool: "demo", Input: "ref", Path: "/etc"}, ErrBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Paths(ctx, "alice", tc.req)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadFileRange(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))

	// The virtual home is always readable.
	home := config.HomeDir(f.configDir, "alice")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "notes.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile("alice", ReadRequest{Path: "/home/alice/notes.md"})
	if err != nil {
		t.Fatalf("home read: %v", err)
	}
	if got.Text != "# hi\n" || got.Path != "/home/alice/notes.md" {
		t.Fatalf("content = %+v", got)
	}

	// A storage the user's tool declares.
	got, err = f.ReadFile("alice", ReadRequest{Path: "/data/readme.txt"})
	if err != nil || got.Text != "hello\n" {
		t.Fatalf("storage read: %+v %v", got, err)
	}

	// A storage no visible tool declares is out of range (the closure again).
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/scratch/anything"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("undeclared storage: %v", err)
	}

	// A workspace path needs the tool that owns it.
	ws := config.WorkspaceDir(f.configDir, "alice", "demo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out.txt"), []byte("out\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/workspace/out.txt"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("a workspace path without a tool must be refused: %v", err)
	}
	if got, err := f.ReadFile("alice", ReadRequest{Path: "/workspace/out.txt", Tool: "demo"}); err != nil || got.Text != "out\n" {
		t.Fatalf("workspace read: %+v %v", got, err)
	}

	// Somewhere else's workspace is not reachable, whatever the tool argument.
	bobWS := config.WorkspaceDir(f.configDir, "bob", "demo")
	if err := os.MkdirAll(bobWS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bobWS, "secret.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/workspace/secret.txt", Tool: "demo"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's workspace: %v", err)
	}

	// Host paths, the tool package and random absolutes are all refused.
	for _, path := range []string{
		"/tool/work.sh",
		"/etc/passwd",
		"/home/bob/notes.md",
		"/data/../../etc/passwd",
		"relative.txt",
	} {
		if _, err := f.ReadFile("alice", ReadRequest{Path: path, Tool: "demo"}); err == nil {
			t.Fatalf("%s must be refused", path)
		}
	}
}

func TestReadFileTextOnlyAndCapped(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))

	home := config.HomeDir(f.configDir, "alice")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/home/alice/blob.bin"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("a NUL byte means binary: %v", err)
	}

	big := strings.Repeat("abcdefghij", 100)
	if err := os.WriteFile(filepath.Join(home, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile("alice", ReadRequest{Path: "/home/alice/big.txt", MaxBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.Text != "abcdefghij" {
		t.Fatalf("cap = %+v", got)
	}

	// A directory is a listing question, not a read.
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/home/alice"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("directory: %v", err)
	}
}

// A symlink inside a writable mount must not turn into a window onto the host.
func TestReadFileRefusesSymlinkEscape(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	home := config.HomeDir(f.configDir, "alice")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(home, "peek")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := f.ReadFile("alice", ReadRequest{Path: "/home/alice/peek"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestValidPrefixTrimsOnlyPartialRunes(t *testing.T) {
	if got := string(validPrefix([]byte("héllo"))); got != "héllo" {
		t.Fatalf("valid text was altered: %q", got)
	}
	// "é" is 0xC3 0xA9; cutting after the first byte leaves a partial rune.
	if got := validPrefix([]byte{'a', 0xC3}); string(got) != "a" {
		t.Fatalf("partial rune not trimmed: %q", got)
	}
	if got := validPrefix([]byte{'a', 0xFF}); string(got) != "a\xff" {
		t.Fatalf("0xFF is not a truncated rune and must survive: %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// instances
// ─────────────────────────────────────────────────────────────────────────

// saveInstance writes a record the way the runtime would, so the read side is
// exercised against real records rather than hand-made structs.
func (f *fixture) saveInstance(t *testing.T, user, toolID, jobID string, mutate func(*runtime.Instance)) *runtime.Instance {
	t.Helper()
	paths := runtime.PathsFor(f.configDir, user, toolID, jobID)
	if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID:        runtime.InstanceID(user, toolID, jobID),
		User:      user,
		Tool:      toolID,
		Kind:      "task",
		JobName:   jobID,
		State:     runtime.StateSucceeded,
		Backend:   "local",
		Sandbox:   "none",
		LogPath:   paths.LogPath,
		WorkDir:   paths.JobDir,
		StartedAt: time.Now().UTC(),
	}
	if jobID == "" {
		inst.Kind = "service"
	}
	if mutate != nil {
		mutate(inst)
	}
	if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestInstancesAreScopedToTheUser(t *testing.T) {
	f := newFixture(t, nil)
	f.saveInstance(t, "alice", "demo", "run-1", nil)
	f.saveInstance(t, "bob", "demo", "run-2", nil)

	views, err := f.Instances("alice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].ID != "alice-demo-run-1" {
		t.Fatalf("alice sees %+v", views)
	}

	// A suffix lookup works, but only inside the caller's own records.
	if _, _, err := f.Instance("alice", "run-1"); err != nil {
		t.Fatalf("suffix lookup: %v", err)
	}
	if _, _, err := f.Instance("alice", "run-2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's instance: %v", err)
	}

	// Ambiguity is an error with the candidates, never a guess.
	f.saveInstance(t, "alice", "other", "run-1", nil)
	if _, ids, err := f.Instance("alice", "run-1"); !errors.Is(err, ErrBadRequest) || len(ids) != 2 {
		t.Fatalf("ambiguous suffix: %v %v", err, ids)
	}

	// Filters narrow the list: a service instance has no job id.
	f.saveInstance(t, "alice", "web", "", nil)
	if k, _ := f.Instances("alice", "", "service"); len(k) != 1 {
		t.Fatalf("kind filter = %+v", k)
	}
	if tool, _ := f.Instances("alice", "web", ""); len(tool) != 1 {
		t.Fatalf("tool filter = %+v", tool)
	}
}

func TestLogsTailAndScoping(t *testing.T) {
	f := newFixture(t, nil)
	inst := f.saveInstance(t, "alice", "demo", "run-1", nil)

	if err := os.MkdirAll(filepath.Dir(inst.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.Logs("alice", "run-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "earlier lines omitted") || !strings.Contains(got, "three") || strings.Contains(got, "one") {
		t.Fatalf("tail = %q", got)
	}
	if _, err := f.Logs("bob", "run-1", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another user's log: %v", err)
	}
	// A missing log is a clear not-found, not an empty string.
	if err := os.Remove(inst.LogPath); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Logs("alice", "run-1", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing log: %v", err)
	}
}

func TestArtifactsResolveDeclaredOutputs(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	ws := config.WorkspaceDir(f.configDir, "alice", "demo")
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "out", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(ws, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	f.saveInstance(t, "alice", "demo", "run-1", func(i *runtime.Instance) {
		i.Outputs = []string{"/workspace/out", "/workspace/missing", "/nope/elsewhere", "/workspace/link"}
	})

	arts, err := f.Artifacts("alice", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 4 {
		t.Fatalf("artifacts = %+v", arts)
	}
	byPath := map[string]Artifact{}
	for _, a := range arts {
		byPath[a.Path] = a
	}
	if a := byPath["/workspace/out"]; !a.Exists || !a.IsDir || a.Entries != 1 {
		t.Fatalf("out = %+v", a)
	}
	if a := byPath["/workspace/missing"]; a.Exists || a.Note != "missing" {
		t.Fatalf("missing = %+v", a)
	}
	if a := byPath["/nope/elsewhere"]; a.Exists || a.Note != "unresolvable" {
		t.Fatalf("unresolvable = %+v", a)
	}
	if a := byPath["/workspace/link"]; a.Exists || !strings.Contains(a.Note, "symlink") {
		t.Fatalf("symlink = %+v", a)
	}

	// A declared path that merely looks like an annotation keeps its name: only
	// the runtime's two exact suffixes are stripped.
	if err := os.MkdirAll(filepath.Join(ws, "out (v2)"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.saveInstance(t, "alice", "demo", "run-3", func(i *runtime.Instance) {
		i.Outputs = []string{"/workspace/out (v2)"}
	})
	arts, err = f.Artifacts("alice", "run-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Path != "/workspace/out (v2)" || !arts[0].Exists {
		t.Fatalf("a versioned directory was mistaken for an annotation: %+v", arts)
	}

	// The runtime's own annotations are understood and re-checked.
	f.saveInstance(t, "alice", "demo", "run-2", func(i *runtime.Instance) {
		i.Outputs = []string{"/workspace/out (missing)"}
	})
	arts, err = f.Artifacts("alice", "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Path != "/workspace/out" || !arts[0].Exists {
		t.Fatalf("annotated output = %+v", arts)
	}
}

// A record whose tool package is gone still answers, with a note instead of an
// error: the record is the user's, and losing the manifest must not hide it.
func TestArtifactsWithoutTheToolPackage(t *testing.T) {
	f := newFixture(t, nil)
	f.saveInstance(t, "alice", "demo", "run-1", func(i *runtime.Instance) {
		i.Outputs = []string{"/workspace/out"}
	})
	if err := os.RemoveAll(filepath.Join(f.toolsDir, "demo")); err != nil {
		t.Fatal(err)
	}
	arts, err := f.Artifacts("alice", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || arts[0].Note == "" || arts[0].Exists {
		t.Fatalf("artifacts = %+v", arts)
	}
	// The instance itself is still readable: the record is the user's.
	if _, _, err := f.Instance("alice", "run-1"); err != nil {
		t.Fatalf("the instance must survive a missing tool package: %v", err)
	}
}

func TestInstanceViewKeepsTheWireContract(t *testing.T) {
	f := newFixture(t, nil)
	f.saveInstance(t, "alice", "demo", "run-1", func(i *runtime.Instance) {
		i.ExitCode = 1
		i.Error = "tool exited with code 1"
		i.Duration = "1.5s"
		i.Tags = map[string]string{"project": "p1"}
	})

	v, _, err := f.Instance("alice", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != "alice-demo-run-1" || v.Tool != "demo" || v.Kind != "task" || v.State != "succeeded" {
		t.Fatalf("view = %+v", v)
	}
	if v.ExitCode != 1 || v.Duration != "1.5s" || v.Tags["project"] != "p1" || v.StartedAt == "" {
		t.Fatalf("view dropped a field: %+v", v)
	}
	if _, _, err := f.Instance("alice", ""); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("empty needle: %v", err)
	}
}

func TestToolViewHidesHostPaths(t *testing.T) {
	f := newFixture(t, nil)
	v, err := f.Tool("alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if v.Entry != "work.sh" || v.Kind != "task" || len(v.Interface.Inputs) != 2 {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Storages) != 1 || v.Storages[0].Root != "/data" {
		t.Fatalf("storages = %+v", v.Storages)
	}
	// The manifest's Dir (a host path) must not be part of the view at all.
	if strings.Contains(v.Entry, f.configDir) {
		t.Fatal("a host path leaked into a tool view")
	}
	if _, err := tool.Find(f.toolsDir, "demo"); err != nil {
		t.Fatal(err)
	}
}
