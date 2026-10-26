package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/runtime"
)

func newHealthServer(t *testing.T, storagesYAML string) *Server {
	t.Helper()
	base := t.TempDir()
	configDir := filepath.Join(base, "config")
	toolsDir := filepath.Join(base, "srcos-tools")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if storagesYAML != "" {
		if err := os.WriteFile(filepath.Join(configDir, "storages.yaml"), []byte(storagesYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	state := &config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 0},
		Auth:   config.AuthState{SessionSecret: "test-secret", SessionTTL: 3600},
	}
	return NewWithOptions(state, configDir, Options{
		ToolsDir:    toolsDir,
		Version:     "test-version",
		TaskWorkers: -1, // no goroutines in a unit test
	})
}

func TestHealthzNeedsNoCredentials(t *testing.T) {
	srv := newHealthServer(t, "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/-/healthz", nil))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/-/healthz", nil))
	if rec.Code != 405 {
		t.Fatalf("POST healthz = %d, want 405", rec.Code)
	}
}

func TestReadyzReportsChecks(t *testing.T) {
	srv := newHealthServer(t, "")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/-/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("readyz = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"ready":true`, `"name":"config"`, `"name":"tools"`, `"name":"task-queue"`} {
		if !strings.Contains(body, want) {
			t.Errorf("readyz missing %s: %s", want, body)
		}
	}
}

func TestReadyzFailsOnAnUnreachableStorage(t *testing.T) {
	srv := newHealthServer(t, `storages:
  - id: gone
    name: Gone
    type: posix
    host_root: /definitely/not/here
    sandbox_path: /data
    mode: ro
`)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/-/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("readyz = %d, want 503 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"ready":false`) || !strings.Contains(rec.Body.String(), `"name":"storages"`) {
		t.Fatalf("readyz body = %s", rec.Body)
	}
}

func TestMetricsExposeInstanceState(t *testing.T) {
	srv := newHealthServer(t, "")
	inst := &runtime.Instance{
		ID: "alice-demo-x", User: "alice", Tool: "demo", Kind: "task",
		State: runtime.StateRunning,
	}
	if err := runtime.SaveInstance(runtime.InstancePath(config.DirOf(srv.registry), inst.ID), inst); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/-/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// Assert whole lines: a literal "\\n" would satisfy a substring check while
	// exposing no metric at all.
	lines := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		lines[line] = true
	}
	for _, want := range []string{
		"srcos_up 1",
		`srcos_build_info{version="test-version"} 1`,
		`srcos_instances{state="running"} 1`,
		"srcos_queue_inflight 0",
	} {
		if !lines[want] {
			t.Errorf("metrics missing the line %q:\n%s", want, body)
		}
	}
}
