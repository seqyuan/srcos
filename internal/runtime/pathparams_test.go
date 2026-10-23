package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

func pathParamTool() *tool.Tool {
	return &tool.Tool{
		ID: "path-tool",
		Interface: tool.Interface{Inputs: []tool.Input{
			{Name: "ref", Type: tool.TypePath, From: "data", Select: "directory"},
			{Name: "reads", Type: tool.TypeFile, From: "data"},
			{Name: "raw", Type: tool.TypeFile},
		}},
	}
}

func pathParamStorage(t *testing.T) (storage.Provider, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ref"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "reads.fq"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := storage.New([]storage.Storage{{
		ID: "data", Kind: storage.KindPosix, HostRoot: root, SandboxPath: "/data", Mode: storage.ReadOnly,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return p, root
}

func TestValidatePathParamsEnforcesTheStorageBinding(t *testing.T) {
	p, _ := pathParamStorage(t)
	tl := pathParamTool()

	ok := &job.Job{Params: map[string]any{"ref": "/data/ref", "reads": "/data/reads.fq"}}
	if err := ValidatePathParams(tl, ok, p); err != nil {
		t.Fatalf("valid selection rejected: %v", err)
	}

	for name, params := range map[string]map[string]any{
		"outside the storage": {"ref": "/etc"},
		"wrong kind":          {"ref": "/data/reads.fq"},
		"directory as file":   {"reads": "/data/ref"},
		"traversal":           {"ref": "/data/../etc"},
	} {
		if err := ValidatePathParams(tl, &job.Job{Params: params}, p); err == nil {
			t.Errorf("%s: invalid selection accepted: %v", name, params)
		}
	}
}

// A raw file/directory input has no storage binding to enforce; the sandbox's
// mount table is its boundary.
func TestValidatePathParamsLeavesUnboundInputsAlone(t *testing.T) {
	tl := pathParamTool()
	if err := ValidatePathParams(tl, &job.Job{Params: map[string]any{"raw": "/workspace/input.txt"}}, nil); err != nil {
		t.Fatalf("unbound input rejected: %v", err)
	}
}

func TestValidateOutputMountsRejectsReadOnlyAndInvisiblePaths(t *testing.T) {
	spec := &sandbox.Spec{}
	if err := spec.Add(sandbox.Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: sandbox.ReadWrite, Origin: "builtin"}); err != nil {
		t.Fatal(err)
	}
	if err := spec.Add(sandbox.Mount{HostPath: "/host/tool", SandboxPath: "/tool", Mode: sandbox.ReadOnly, Origin: "tool"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOutputMounts(spec, []string{"/workspace/out"}, false); err != nil {
		t.Fatalf("writable output rejected: %v", err)
	}
	for _, out := range []string{"/tool/report.txt", "/etc/passwd"} {
		if err := ValidateOutputMounts(spec, []string{out}, false); err == nil {
			t.Errorf("output %q was accepted", out)
		}
	}
}
