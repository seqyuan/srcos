package environment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/sandbox"
)

func TestNewValidatesDeclarations(t *testing.T) {
	good := Environment{
		ID:   "r-miniforge",
		Root: "/opt/r",
		Env:  []string{"PATH=/opt/srcos/bin:/opt/r/bin:/usr/bin"},
	}
	if _, err := New([]Environment{good}); err != nil {
		t.Fatalf("a well-formed environment must be accepted: %v", err)
	}

	for _, tc := range []struct {
		name string
		e    Environment
		want string
	}{
		{"bad id", Environment{ID: "R Miniforge", Root: "/opt/r"}, "must match"},
		{"relative root", Environment{ID: "r", Root: "opt/r"}, "absolute"},
		{"malformed env entry", Environment{ID: "r", Root: "/opt/r", Env: []string{"NOVALUE"}}, "VAR=VALUE"},
		{"PATH without the platform bin", Environment{ID: "r", Root: "/opt/r", Env: []string{"PATH=/usr/bin:/bin"}}, sandbox.PathToolBin},
		{"provides with a path", Environment{ID: "r", Root: "/opt/r", Provides: []string{"bin/R"}}, "bare executable name"},
	} {
		if _, err := New([]Environment{tc.e}); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	// Duplicate ids are a configuration mistake, not a silent last-wins.
	if _, err := New([]Environment{good, good}); err == nil {
		t.Fatal("a duplicate environment id must be rejected")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "environments.yaml"))
	if err != nil {
		t.Fatalf("a missing file means 'none declared': %v", err)
	}
	if len(p.List()) != 0 {
		t.Fatalf("List = %+v, want empty", p.List())
	}
	if _, ok := p.Get("r-miniforge"); ok {
		t.Fatal("an empty provider must not answer any id")
	}
}

func TestCheckOneLooksAtTheHost(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "R"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	e := Environment{ID: "r", Root: root, Provides: []string{"R"}}
	if problems := CheckOne(e); len(problems) != 0 {
		t.Fatalf("a reachable environment must have no problems: %v", problems)
	}

	// A missing provided executable is reported with its path.
	e.Provides = []string{"Rscript"}
	if problems := CheckOne(e); len(problems) != 1 || !strings.Contains(problems[0], "Rscript") {
		t.Fatalf("missing provides = %v", problems)
	}

	// A root that is not there is reported once, not chased further.
	e.Root = filepath.Join(root, "nope")
	e.Provides = []string{"R"}
	if problems := CheckOne(e); len(problems) != 1 || !strings.Contains(problems[0], "not readable") {
		t.Fatalf("missing root = %v", problems)
	}
}

// The root is mounted read-only *at its own host path* — interpreters hardcode
// absolute paths, so mounting the prefix elsewhere breaks them.
func TestMountsForKeepsHostPathAndReadOnly(t *testing.T) {
	spec := &sandbox.Spec{}
	if err := MountsFor(spec, []Environment{{ID: "r", Root: "/opt/r"}}); err != nil {
		t.Fatal(err)
	}
	mounts := spec.Mounts()
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v, want one", mounts)
	}
	m := mounts[0]
	if m.HostPath != "/opt/r" || m.SandboxPath != "/opt/r" {
		t.Fatalf("mount = %+v, want host path == sandbox path", m)
	}
	if m.Mode != sandbox.ReadOnly {
		t.Fatalf("mode = %v, want read-only (environment is not data)", m.Mode)
	}
	if m.Origin != OriginFor("r") {
		t.Fatalf("origin = %q, want %q", m.Origin, OriginFor("r"))
	}
}

func TestListIsSorted(t *testing.T) {
	p, err := New([]Environment{{ID: "zeta", Root: "/z"}, {ID: "alpha", Root: "/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.List(); got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Fatalf("List = %+v, want sorted by id", got)
	}
}
