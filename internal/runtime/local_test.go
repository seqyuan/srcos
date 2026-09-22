package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// harness wires a throwaway config dir, a tool package and one submission, so
// a test can exercise the whole local path: validate -> materialize -> run ->
// record.
type harness struct {
	configDir string
	toolsDir  string
	toolDir   string
	user      string
	runner    *Runner
}

func newHarness(t *testing.T, sandboxMode string, workSh string, extraToolYAML string) *harness {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toolsDir := filepath.Join(root, "tools")
	toolDir := filepath.Join(toolsDir, "demo")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: ` + sandboxMode + `
entry: work.sh
interface:
  inputs:
    - {name: msg, type: string, default: "hello"}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 1, memory: "256Mi", walltime: "0:01:00"}
` + extraToolYAML
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "work.sh"), []byte(workSh), 0o755); err != nil {
		t.Fatal(err)
	}

	// A workspace template proves init_from runs exactly once.
	tmpl := filepath.Join(toolDir, "workspace-template")
	if err := os.MkdirAll(tmpl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpl, "seeded.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// And the manifest asks for it.
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"),
		[]byte(manifest+"workspace:\n  init_from: workspace-template/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	return &harness{
		configDir: configDir,
		toolsDir:  toolsDir,
		toolDir:   toolDir,
		user:      "alice",
		runner: NewRunner(Options{
			ConfigDir: configDir,
			ToolsDir:  toolsDir,
			User:      "alice",
			// No StorageProvider: every fixture here declares no storages, and
			// a tool that did would fail loudly rather than silently.
			Backends: map[string]Backend{"local": &Local{SystemdUser: boolPtr(false)}},
		}),
	}
}

func (h *harness) submit(t *testing.T, id string, body string) *job.Loaded {
	t.Helper()
	tl, err := tool.Load(h.toolDir)
	if err != nil {
		t.Fatalf("tool.Load: %v", err)
	}
	dir := filepath.Join(config.JobsDir(h.configDir, h.user, tl.ID), id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "job.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := job.Load(dir)
	if err != nil {
		t.Fatalf("job.Load: %v", err)
	}
	return &job.Loaded{ID: id, Dir: dir, Path: filepath.Join(dir, "job.json"), Job: loaded}
}

func (h *harness) tool(t *testing.T) *tool.Tool {
	t.Helper()
	tl, err := tool.Load(h.toolDir)
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

const okScript = `#!/usr/bin/env bash
set -euo pipefail
OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID}"
mkdir -p "${OUT}"
printf '%s\n' "${SRCOS_PARAM_MSG}" > "${OUT}/msg.txt"
printf 'home=%s workspace=%s cwd=%s\n' "$HOME" "$SRCOS_WORKSPACE" "$PWD" > "${OUT}/env.txt"
touch "${OUT}/.sign"
echo "[done]"
`

const failScript = `#!/usr/bin/env bash
echo "about to fail" >&2
exit 7
`

const idempotentScript = `#!/usr/bin/env bash
set -euo pipefail
OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID}"
SIGN="${OUT}/.sign"
if [[ -f "${SIGN}" ]]; then echo "[skip]"; exit 0; fi
mkdir -p "${OUT}"; touch "${SIGN}"; echo "[first run]"
`

// requireBwrap skips the test when bubblewrap cannot start a sandbox here.
// This is the difference between "the code is wrong" and "this host cannot".
func requireBwrap(t *testing.T) {
	t.Helper()
	if _, ok, why := sandbox.BwrapProbe(); !ok {
		t.Skipf("bubblewrap unavailable: %s", why)
	}
}

func TestRunSandboxNoneSucceeds(t *testing.T) {
	// sandbox: none always runs, so this test is the baseline that other
	// failures can be compared against.
	h := newHarness(t, "none", okScript, "")
	loaded := h.submit(t, "j1", `{"schemaVersion":1,"name":"demo","params":{"msg":"hi"},"outputs":["/workspace/out"]}`)

	inst, err := h.runner.RunTask(context.Background(), h.tool(t), loaded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inst.State != StateSucceeded {
		t.Fatalf("state = %s (%s)", inst.State, inst.Error)
	}
	if inst.ExitCode != 0 {
		t.Fatalf("exit = %d", inst.ExitCode)
	}

	// The tool-level default must have reached the script.
	msg := readFile(t, filepath.Join(config.WorkspaceDir(h.configDir, h.user, "demo"), "out", "j1", "msg.txt"))
	if strings.TrimSpace(msg) != "hi" {
		t.Fatalf("msg = %q", msg)
	}

	// init_from ran, so the seeded file is present.
	if _, err := os.Stat(filepath.Join(config.WorkspaceDir(h.configDir, h.user, "demo"), "seeded.txt")); err != nil {
		t.Fatalf("workspace template was not applied: %v", err)
	}

	// The record is on disk and reloads.
	saved, err := LoadInstance(InstancePath(h.configDir, InstanceID("alice", "demo", "j1")))
	if err != nil {
		t.Fatalf("instance record: %v", err)
	}
	if saved.ID != inst.ID || saved.State != StateSucceeded {
		t.Fatalf("saved record mismatch: %+v", saved)
	}

	// The log lives outside the workspace so the tool cannot rewrite it.
	if !strings.HasPrefix(inst.LogPath, filepath.Join(config.DataDir(h.configDir), "logs")) {
		t.Fatalf("log path %q should be under data/logs", inst.LogPath)
	}
	if !strings.Contains(readFile(t, inst.LogPath), "[done]") {
		t.Fatal("log does not contain the tool's output")
	}
}

func TestRunWithBwrapIsolatesWorkspace(t *testing.T) {
	requireBwrap(t)
	h := newHarness(t, "bwrap", okScript, "")
	loaded := h.submit(t, "j2", `{"schemaVersion":1,"name":"demo","params":{"msg":"sandboxed"},"outputs":["/workspace/out"]}`)

	inst, err := h.runner.RunTask(context.Background(), h.tool(t), loaded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inst.State != StateSucceeded {
		t.Fatalf("state = %s (%s)\nlog:\n%s", inst.State, inst.Error, readFile(t, inst.LogPath))
	}

	env := readFile(t, filepath.Join(config.WorkspaceDir(h.configDir, h.user, "demo"), "out", "j2", "env.txt"))
	for _, want := range []string{
		"home=/home/alice", // the *virtual* home, not the OS user's
		"workspace=/workspace",
		"cwd=/workspace/jobs/j2", // the job root is the cwd
	} {
		if !strings.Contains(env, want) {
			t.Errorf("sandbox environment missing %q; got:\n%s", want, env)
		}
	}

	// The mounts must be exact paths, never a parent directory.
	for _, m := range inst.Mounts {
		parts := strings.Split(m, ":")
		if len(parts) >= 2 && parts[1] == "/" {
			t.Fatalf("the host root must never be mounted: %s", m)
		}
	}
}

func TestRunNonZeroExitIsDataNotError(t *testing.T) {
	h := newHarness(t, "none", failScript, "")
	loaded := h.submit(t, "j3", `{"schemaVersion":1,"name":"demo"}`)

	inst, err := h.runner.RunTask(context.Background(), h.tool(t), loaded)
	if err != nil {
		t.Fatalf("a tool failure must not be a SRCOS error: %v", err)
	}
	if inst.State != StateFailed {
		t.Fatalf("state = %s", inst.State)
	}
	if inst.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", inst.ExitCode)
	}
	if !strings.Contains(readFile(t, inst.LogPath), "about to fail") {
		t.Fatal("stderr must be captured into the log")
	}
}

// TestRunIsIdempotentPerTask pins the lesson from the first end-to-end run: the
// .sign marker must be per task, so a second task with different parameters is
// not mistaken for one that already ran.
func TestRunIsIdempotentPerTask(t *testing.T) {
	h := newHarness(t, "none", idempotentScript, "")
	tl := h.tool(t)

	for i, id := range []string{"taskA", "taskB"} {
		loaded := h.submit(t, id, `{"schemaVersion":1,"name":"demo"}`)
		inst, err := h.runner.RunTask(context.Background(), tl, loaded)
		if err != nil {
			t.Fatal(err)
		}
		if inst.State != StateSucceeded {
			t.Fatalf("%s: %s (%s)", id, inst.State, inst.Error)
		}
		log := readFile(t, inst.LogPath)
		if i == 0 && !strings.Contains(log, "[first run]") {
			t.Fatalf("first task should have run, log: %s", log)
		}
		if i == 1 && strings.Contains(log, "[skip]") {
			t.Fatalf("a second, distinct task must not hit the first task's marker; log: %s", log)
		}
	}

	// Re-running the *same* task skips: that is the marker doing its job.
	loaded := &job.Loaded{ID: "taskA", Dir: filepath.Join(config.JobsDir(h.configDir, h.user, "demo"), "taskA")}
	j, err := job.Load(loaded.Dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Job = j
	inst, err := h.runner.RunTask(context.Background(), tl, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, inst.LogPath), "[skip]") {
		t.Fatalf("re-running the same task should skip; log: %s", readFile(t, inst.LogPath))
	}
}

func TestRunRejectsUnimplementedFeaturesClearly(t *testing.T) {
	h := newHarness(t, "none", okScript, "requires_storages: [cluster-share]\n")
	loaded := h.submit(t, "j4", `{"schemaVersion":1,"name":"demo"}`)

	_, err := h.runner.RunTask(context.Background(), h.tool(t), loaded)
	if err == nil {
		t.Fatal("a tool requiring storages must fail loudly until the provider exists")
	}
	if !strings.Contains(err.Error(), "StorageProvider") {
		t.Fatalf("the error should name the missing seam: %v", err)
	}
}

func TestUnusableSandboxProducesFailedInstance(t *testing.T) {
	h := newHarness(t, "apptainer", okScript, `image: /nonexistent/x.sif
`)
	loaded := h.submit(t, "j5", `{"schemaVersion":1,"name":"demo"}`)

	inst, err := h.runner.RunTask(context.Background(), h.tool(t), loaded)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inst.State != StateFailed {
		t.Fatalf("state = %s", inst.State)
	}
	if !strings.Contains(inst.Error, "Phase 6") {
		t.Fatalf("unexpected error: %s", inst.Error)
	}
	// The failure must be recorded, not just printed.
	if _, err := LoadInstance(InstancePath(h.configDir, InstanceID("alice", "demo", "j5"))); err != nil {
		t.Fatalf("failed instances must still be recorded: %v", err)
	}
}

func TestListInstancesNewestFirst(t *testing.T) {
	h := newHarness(t, "none", okScript, "")
	tl := h.tool(t)
	for _, id := range []string{"a", "b", "c"} {
		loaded := h.submit(t, id, `{"schemaVersion":1,"name":"demo"}`)
		if _, err := h.runner.RunTask(context.Background(), tl, loaded); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ListInstances(h.configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("expected 3 instances, got %d", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].StartedAt.Before(list[i].StartedAt) {
			t.Fatalf("instances are not newest-first: %v", []string{list[0].ID, list[1].ID, list[2].ID})
		}
	}
}

func TestInitFromTemplateOnlyOnce(t *testing.T) {
	target := t.TempDir()
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "a.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitFromTemplate(tmpl, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".srcos-initialized")); err != nil {
		t.Fatal("the marker must be written")
	}
	// A user deleting a seeded file must not have it restored.
	if err := os.Remove(filepath.Join(target, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := InitFromTemplate(tmpl, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "a.txt")); err == nil {
		t.Fatal("init_from must not re-seed over a user's deliberate deletion")
	}
}

func TestInitFromTemplateMissingDir(t *testing.T) {
	err := InitFromTemplate(filepath.Join(t.TempDir(), "nope"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected a clear error, got %v", err)
	}
}

// TestDetectLimiterNeverPanics checks that limiter selection is total: on any
// host one of the three names comes back.
func TestBuildLimiterNeverPanics(t *testing.T) {
	for _, res := range []tool.Resources{
		{},
		{CPU: 4, Memory: "1Gi"},
		{CPU: 1, Memory: "not-a-size"},
	} {
		if lim := BuildLimiter(res); lim.Name != "" {
			t.Fatalf("Limiter.Name should stay empty (the backend picks the mechanism), got %q", lim.Name)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
