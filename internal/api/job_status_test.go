package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/runtime"
)

// writeArtifactInstance drops an instance record plus the files it declared as
// outputs, in the layout the runtime uses (data/ws/<user>/<tool>).
func writeArtifactInstance(t *testing.T, h *toolsHarness, id string) string {
	t.Helper()
	workspace := config.WorkspaceDir(h.configDir, "alice", "demo")
	outDir := filepath.Join(workspace, "out", "run-1")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Minute)
	inst := &runtime.Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   id,
		State:     runtime.StateSucceeded,
		ExitCode:  0,
		Sandbox:   "bwrap",
		Backend:   "local",
		StartedAt: started,
		EndedAt:   started.Add(42 * time.Second),
		Duration:  "42s",
		// The parent directory is what a task declares; the per-run subdirectory
		// is the idempotency unit inside it.
		Outputs: []string{"/workspace/out"},
		Tags:    map[string]string{"sample": "S001"},
		LogPath: filepath.Join(t.TempDir(), id+".log"),
	}
	if err := runtime.SaveInstance(runtime.InstancePath(h.configDir, id), inst); err != nil {
		t.Fatal(err)
	}
	return outDir
}

func TestJobStatusIsThePollingContract(t *testing.T) {
	h := newToolsHarness(t, nil)
	writeArtifactInstance(t, h, "alice-demo-run1")

	rec := h.get(t, "/api/jobs/alice-demo-run1")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var got jobStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if got.State != "succeeded" || got.ExitCode != 0 || got.Kind != "task" {
		t.Errorf("unexpected status: %+v", got)
	}
	if got.StartedAt == "" || got.EndedAt == "" || got.Duration != "42s" {
		t.Errorf("timestamps/duration missing: %+v", got)
	}
	if len(got.Outputs) != 1 || got.Outputs[0] != "/workspace/out" {
		t.Errorf("outputs = %v", got.Outputs)
	}
	// The links are what lets a page follow the answer without building paths.
	if got.Links.Logs != "/api/jobs/alice-demo-run1/logs" ||
		got.Links.Artifacts != "/api/jobs/alice-demo-run1/artifacts" ||
		got.Links.Cancel != "/api/jobs/alice-demo-run1/cancel" {
		t.Errorf("links = %+v", got.Links)
	}
}

func TestJobStatusRefusesUnknownAndMalformed(t *testing.T) {
	h := newToolsHarness(t, nil)
	writeArtifactInstance(t, h, "alice-demo-run1")

	for _, url := range []string{
		"/api/jobs/alice-demo-nope",
		"/api/jobs/alice-demo-run1/",
		"/api/jobs/alice-demo-run1/other",
		"/api/jobs/..%2F..%2Fetc",
	} {
		if rec := h.get(t, url); rec.Code != 404 {
			t.Errorf("%s = %d, want 404 (%s)", url, rec.Code, rec.Body)
		}
	}
}

func TestJobArtifactsListsDeclaredOutputs(t *testing.T) {
	h := newToolsHarness(t, nil)
	writeArtifactInstance(t, h, "alice-demo-run1")

	rec := h.get(t, "/api/jobs/alice-demo-run1/artifacts")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Artifacts []struct {
			Path    string `json:"path"`
			Exists  bool   `json:"exists"`
			IsDir   bool   `json:"isDir"`
			Entries int    `json:"entries"`
			Addr    string `json:"addr"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", got.Artifacts)
	}
	a := got.Artifacts[0]
	if a.Path != "/workspace/out" || !a.Exists || !a.IsDir || a.Entries != 1 {
		t.Errorf("artifact = %+v", a)
	}
	// The workspace is a browsable scope, so the viewer address must be there.
	if a.Addr != "srcos://file/workspace/out?tool=demo" {
		t.Errorf("addr = %q", a.Addr)
	}
}

func TestJobArtifactsUnknownInstance(t *testing.T) {
	h := newToolsHarness(t, nil)
	if rec := h.get(t, "/api/jobs/alice-demo-nope/artifacts"); rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
