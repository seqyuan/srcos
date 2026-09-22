package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/sandbox"
)

func mustNew(t *testing.T, items ...Storage) Provider {
	t.Helper()
	p, err := New(items)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestNewValidates(t *testing.T) {
	cases := []struct {
		name    string
		items   []Storage
		wantSub string
	}{
		{
			name:    "relative host root",
			items:   []Storage{{ID: "a", Kind: KindPosix, HostRoot: "share", SandboxPath: "/data"}},
			wantSub: "host_root must be an absolute host path",
		},
		{
			name:    "relative sandbox path",
			items:   []Storage{{ID: "a", Kind: KindPosix, HostRoot: "/share", SandboxPath: "data"}},
			wantSub: "sandbox_path must be absolute",
		},
		{
			name:    "reserved sandbox path",
			items:   []Storage{{ID: "a", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/usr/local/share"}},
			wantSub: "read-only system directory",
		},
		{
			name:    "unknown kind",
			items:   []Storage{{ID: "a", Kind: "nfs", HostRoot: "/share", SandboxPath: "/data"}},
			wantSub: "unknown type",
		},
		{
			name:    "s3 not implemented",
			items:   []Storage{{ID: "a", Kind: KindS3, HostRoot: "/share", SandboxPath: "/data"}},
			wantSub: "not implemented yet",
		},
		{
			name:    "bad mode",
			items:   []Storage{{ID: "a", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data", Mode: "rx"}},
			wantSub: "mode must be ro|rw",
		},
		{
			name: "duplicate id",
			items: []Storage{
				{ID: "a", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data"},
				{ID: "a", Kind: KindPosix, HostRoot: "/share2", SandboxPath: "/data2"},
			},
			wantSub: "duplicate id",
		},
		{
			// The nested-storage case is the data-exposure mistake: a parent
			// root hands over every group-readable directory beneath it.
			name: "overlapping sandbox paths",
			items: []Storage{
				{ID: "all", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data"},
				{ID: "proj", Kind: KindPosix, HostRoot: "/share/projA", SandboxPath: "/data/projA"},
			},
			wantSub: "overlap",
		},
		{
			name:    "bad id",
			items:   []Storage{{ID: "Bad_ID", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data"}},
			wantSub: "id must match",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.items)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestNewDefaultsModeToReadOnly(t *testing.T) {
	// Forgetting a mode must not produce a writable mount.
	p := mustNew(t, Storage{ID: "a", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data"})
	got, _ := p.Get("a")
	if got.Mode != ReadOnly {
		t.Fatalf("mode = %q, want ro", got.Mode)
	}
	if got.Name != "a" {
		t.Fatalf("name should fall back to the id, got %q", got.Name)
	}
}

func TestEmptyConfigIsValid(t *testing.T) {
	// A deployment with no shared data is legitimate; tools that ask for a
	// storage then fail loudly at registration instead.
	p := mustNew(t)
	if len(p.List()) != 0 {
		t.Fatal("expected no storages")
	}
	if _, err := p.ForTool([]string{"anything"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestForToolResolvesAndRejects(t *testing.T) {
	p := mustNew(t,
		Storage{ID: "data", Kind: KindPosix, HostRoot: "/share", SandboxPath: "/data"},
		Storage{ID: "proj", Kind: KindPosix, HostRoot: "/share/projA", SandboxPath: "/proj", Mode: ReadWrite},
	)
	got, err := p.ForTool([]string{"data", "proj"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d storages", len(got))
	}

	// An unknown id must name what is available, so the operator can fix it.
	_, err = p.ForTool([]string{"data", "typo"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "data") {
		t.Fatalf("the error should list the known ids: %v", err)
	}
}

func TestResolveIsJailChecked(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ref"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	host, err := p.Resolve("data", "/data/ref")
	if err != nil {
		t.Fatal(err)
	}
	if host != filepath.Join(root, "ref") {
		t.Fatalf("Resolve = %q", host)
	}

	// Escapes and cross-storage lookups are both refused.
	for _, bad := range []string{"/data/../../etc/passwd", "/etc/passwd", "/", "/datax"} {
		if _, err := p.Resolve("data", bad); err == nil {
			t.Errorf("Resolve(%q) should fail", bad)
		}
	}
	if _, err := p.Resolve("nope", "/data"); !errors.Is(err, ErrNotFound) {
		t.Error("an unknown storage id must be ErrNotFound")
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	if _, err := p.Resolve("data", "/data/escape"); err == nil {
		t.Fatal("a symlink leading outside the storage must be rejected")
	}
}

func TestListingEnumerates(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	got, err := p.Listing(context.Background(), "data", "/data", SelectAny, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 {
		t.Fatalf("entries = %+v", got.Entries)
	}
	// Directories first, then case-insensitive by name.
	if !got.Entries[0].IsDir || got.Entries[1].Name != "a.txt" || got.Entries[2].Name != "b.txt" {
		t.Fatalf("unexpected order: %+v", got.Entries)
	}
	// Paths are in the sandbox's space, directly usable by a tool.
	if got.Entries[1].Path != "/data/a.txt" {
		t.Fatalf("entry path = %q", got.Entries[1].Path)
	}
}

func TestListingHonoursSelect(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	dirs, err := p.Listing(context.Background(), "data", "/data", SelectDirectory, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs.Entries) != 1 || dirs.Entries[0].Name != "d" {
		t.Fatalf("directory filter leaked files: %+v", dirs.Entries)
	}

	files, err := p.Listing(context.Background(), "data", "/data", SelectFile, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Entries) != 1 || files.Entries[0].Name != "f.txt" {
		t.Fatalf("file filter leaked directories: %+v", files.Entries)
	}
}

func TestListingTruncates(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(filepath.Join(root, string(rune('a'+i))+".txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	got, err := p.Listing(context.Background(), "data", "/data", SelectAny, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 3 || !got.Truncated {
		t.Fatalf("expected 3 truncated entries, got %d truncated=%v", len(got.Entries), got.Truncated)
	}
}

func TestListingDefaultsToStorageRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})

	got, err := p.Listing(context.Background(), "data", "", SelectAny, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/data" || len(got.Entries) != 1 {
		t.Fatalf("unexpected listing: %+v", got)
	}
}

func TestListingRejectsOutsidePaths(t *testing.T) {
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: t.TempDir(), SandboxPath: "/data"})
	for _, bad := range []string{"/etc", "/data/..", "/workspace"} {
		if _, err := p.Listing(context.Background(), "data", bad, SelectAny, 10); err == nil {
			t.Errorf("Listing(%q) should fail", bad)
		}
	}
}

func TestListingRejectsFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})
	if _, err := p.Listing(context.Background(), "data", "/data/f.txt", SelectAny, 10); err == nil {
		t.Fatal("listing a file should fail")
	}
}

func TestListingSkipsDanglingSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})
	got, err := p.Listing(context.Background(), "data", "/data", SelectAny, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 0 {
		t.Fatalf("a dangling symlink must not be listed: %+v", got.Entries)
	}
}

func TestDisplayIsInverse(t *testing.T) {
	root := t.TempDir()
	p := mustNew(t, Storage{ID: "data", Kind: KindPosix, HostRoot: root, SandboxPath: "/data"})
	got, err := p.Display(filepath.Join(root, "a", "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "/data/a/b.txt" {
		t.Fatalf("Display = %q", got)
	}
}

// sandboxSpec records the mounts MountsFor produced by rendering them the
// same way the runtime does, so the test asserts on the real argv shape.
type sandboxSpec struct {
	spec sandbox.Spec
}

func (s *sandboxSpec) args() []string {
	return sandbox.BwrapArgv(&s.spec, sandbox.BwrapOptions{Argv: []string{"/bin/true"}})
}

func TestMountsForRendersModes(t *testing.T) {
	spec := &sandboxSpec{}
	if err := MountsFor(&spec.spec, []Storage{
		{ID: "data", HostRoot: "/share", SandboxPath: "/data", Mode: ReadOnly},
		{ID: "proj", HostRoot: "/share/p", SandboxPath: "/proj", Mode: ReadWrite},
	}); err != nil {
		t.Fatal(err)
	}
	args := spec.args()
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--ro-bind /share /data") {
		t.Errorf("ro storage should use --ro-bind: %s", joined)
	}
	if !strings.Contains(joined, "--bind /share/p /proj") {
		t.Errorf("rw storage should use --bind: %s", joined)
	}
	// The origin keeps the storage id, which is how Resolve recovers it.
	// Origin is metadata rather than argv, so it is asserted on the mount list.
	var origins []string
	for _, m := range spec.spec.Mounts() {
		origins = append(origins, m.Origin)
	}
	if strings.Join(origins, ",") != "storage:data,storage:proj" {
		t.Fatalf("origins = %v", origins)
	}
}

func TestCheckReachableReportsMissingRoot(t *testing.T) {
	p := mustNew(t, Storage{ID: "gone", Kind: KindPosix, HostRoot: "/definitely/not/here", SandboxPath: "/data"})
	checker, ok := p.(ReachabilityChecker)
	if !ok {
		t.Fatal("provider should expose CheckReachable")
	}
	problems := checker.CheckReachable()
	if len(problems) != 1 {
		t.Fatalf("expected one problem, got %v", problems)
	}
	// The message must tell the operator how to fix it, since bind mounts do
	// not change permissions.
	if !strings.Contains(problems[0], "setfacl") {
		t.Fatalf("unhelpful message: %s", problems[0])
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "storages.yaml"))
	if err != nil {
		t.Fatalf("a missing storages.yaml is not an error: %v", err)
	}
	if len(p.List()) != 0 {
		t.Fatal("expected empty provider")
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "share")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "storages.yaml")
	body := "storages:\n" +
		"  - id: data\n" +
		"    name: 共享数据盘\n" +
		"    type: posix\n" +
		"    host_root: " + root + "\n" +
		"    sandbox_path: /data\n" +
		"    mode: ro\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := p.Get("data")
	if !ok || got.Name != "共享数据盘" || got.SandboxPath != "/data" {
		t.Fatalf("unexpected storage: %+v", got)
	}
}
