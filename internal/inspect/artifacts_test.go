package inspect

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/runtime"
)

// A declared output is only useful if a reader can get to it, so every artifact
// that lives inside a browsable scope carries a srcos:// address. This test
// pins the mapping: workspace → workspace scope (with the tool), storage → the
// storage id, home → home, and anything else (the tool package) → no address.
func TestArtifactsCarryResourceAddresses(t *testing.T) {
	f := newFixture(t, allowOnly(t, "demo"))
	seedResourceFixture(t, f)

	// A directory in the workspace, so the artifact resolves and is a directory.
	ws := config.WorkspaceDir(f.configDir, "alice", "demo")
	if err := os.MkdirAll(filepath.Join(ws, "out"), 0o755); err != nil {
		t.Fatal(err)
	}

	id := "alice-demo-s01"
	rec := &runtime.Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   "s01",
		State:     runtime.StateSucceeded,
		LogPath:   filepath.Join(t.TempDir(), "s01.log"),
		StartedAt: time.Now().UTC(),
		Outputs: []string{
			"/workspace/out",
			"/data/ref/genes.tsv",
			"/home/alice/notes.md",
			"/tool/tool.yaml",
			"/flow/runs/x/y", // builtin but not a browsable scope
		},
	}
	if err := runtime.SaveInstance(runtime.InstancePath(f.configDir, id), rec); err != nil {
		t.Fatal(err)
	}

	artifacts, err := f.Artifacts("alice", id)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Artifact{}
	for _, a := range artifacts {
		byPath[a.Path] = a
	}

	want := map[string]string{
		"/workspace/out":       "srcos://file/workspace/out?tool=demo",
		"/data/ref/genes.tsv":  "srcos://file/data/ref/genes.tsv",
		"/home/alice/notes.md": "srcos://file/home/notes.md",
		"/tool/tool.yaml":      "",
		"/flow/runs/x/y":       "",
	}
	for path, wantAddr := range want {
		a, ok := byPath[path]
		if !ok {
			t.Errorf("artifact %s is missing from %+v", path, artifacts)
			continue
		}
		if a.Addr != wantAddr {
			t.Errorf("artifact %s addr = %q, want %q", path, a.Addr, wantAddr)
		}
	}

	if a := byPath["/workspace/out"]; !a.Exists || !a.IsDir {
		t.Errorf("workspace artifact = %+v, want an existing directory", a)
	}
	if a := byPath["/data/ref/genes.tsv"]; !a.Exists || a.IsDir {
		t.Errorf("storage artifact = %+v, want an existing file", a)
	}
	if a := byPath["/flow/runs/x/y"]; a.Exists {
		t.Errorf("a path that was never created must not exist: %+v", a)
	}
}
