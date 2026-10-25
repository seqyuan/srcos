package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared package puts the UI and the task in sibling components and points
// both at the package root. SRCOS mounts that root at /tool; the component's
// entry stays manifest-relative (work.sh -> /tool/task/work.sh).
func TestSharedPackageRoot(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "scrna")
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(pkg, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	component := func(id string) string {
		return `
schemaVersion: 1
id: ` + id + `
version: 0.1.0
name: ` + id + `
kind: task
backend: local
root: ..
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`
	}
	write("task/tool.yaml", component("scrna-task"))
	write("task/work.sh", "#!/bin/sh\nexit 0\n")
	write("ui/tool.yaml", component("scrna-ui"))
	write("ui/work.sh", "#!/bin/sh\nexit 0\n")
	write("shared/lib.R", "# shared\n")

	tools, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("Discover found %d tools, want 2", len(tools))
	}
	for _, tl := range tools {
		if tl.PackageRootDir() != pkg {
			t.Errorf("%s: PackageRoot = %q, want %q", tl.ID, tl.PackageRootDir(), pkg)
		}
		wantEntry := filepath.Join(tl.Dir, "work.sh")
		gotEntry := filepath.Join(tl.PackageRootDir(), filepath.FromSlash(tl.EntryRel))
		if gotEntry != wantEntry {
			t.Errorf("%s: EntryRel %q resolves to %q, want %q", tl.ID, tl.EntryRel, gotEntry, wantEntry)
		}
	}

	// Find's fast path only covers <root>/<id>; these ids live in components,
	// so Find must fall back to Discover.
	for _, id := range []string{"scrna-task", "scrna-ui"} {
		tl, err := Find(root, id)
		if err != nil {
			t.Fatalf("Find(%s): %v", id, err)
		}
		if tl.ID != id || tl.PackageRootDir() != pkg {
			t.Errorf("Find(%s) = %+v", id, tl)
		}
	}
}

func TestPackageRootMustStayInsideToolsDir(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "scrna")
	if err := os.MkdirAll(filepath.Join(pkg, "task"), 0o755); err != nil {
		t.Fatal(err)
	}
	// ../.. is the parent of the tools dir: mounting it would expose other
	// packages (or arbitrary host data) at /tool.
	manifest := `
schemaVersion: 1
id: scrna-task
version: 0.1.0
name: task
kind: task
backend: local
root: ../..
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`
	if err := os.WriteFile(filepath.Join(pkg, "task", "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "task", "work.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "must be below the tools directory") {
		t.Fatalf("Discover error = %v, want a package-root escape", err)
	}
}

func TestEntryMustStayInsidePackageRoot(t *testing.T) {
	dir := writeTool(t, map[string]string{
		"tool.yaml": `
schemaVersion: 1
id: demo
version: 0.1.0
name: demo
kind: task
backend: local
root: .
entry: ../outside.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
	})
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "escapes the package root") {
		t.Fatalf("Load error = %v, want an entry escape", err)
	}
}

func TestRootMustBeRelative(t *testing.T) {
	dir := writeTool(t, map[string]string{
		"tool.yaml": `
schemaVersion: 1
id: demo
version: 0.1.0
name: demo
kind: task
backend: local
root: /tmp
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
	})
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "must be relative") {
		t.Fatalf("Load error = %v, want a relative-root error", err)
	}
}

// version is a human label, not a registration requirement: SRCOS only
// registers and references the tool; version control lives outside.
func TestVersionIsOptional(t *testing.T) {
	dir := writeTool(t, map[string]string{
		"tool.yaml": `
schemaVersion: 1
id: demo
name: demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
		"work.sh": "#!/bin/sh\nexit 0\n",
	})
	if _, err := Load(dir); err != nil {
		t.Fatalf("Load without version: %v", err)
	}
}

func TestRootMustBeAnAncestor(t *testing.T) {
	root := t.TempDir()
	pkg := filepath.Join(root, "scrna")
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(filepath.Join(pkg, "task"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `
schemaVersion: 1
id: scrna-task
name: task
kind: task
backend: local
root: ../../other
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`
	if err := os.WriteFile(filepath.Join(pkg, "task", "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "task", "work.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(pkg, "task")); err == nil || !strings.Contains(err.Error(), "ancestor") {
		t.Fatalf("Load error = %v, want a non-ancestor root error", err)
	}
}
