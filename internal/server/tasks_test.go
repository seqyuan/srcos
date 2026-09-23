package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
)

// tasksGateway is proxyGateway plus one finished instance with a log, which is
// enough for both the list and the detail page.
func tasksGateway(t *testing.T) (*Server, string) {
	t.Helper()
	srv, configDir := proxyGateway(t)
	id := "alice-demo-s01"
	logPath := filepath.Join(t.TempDir(), "s01.log")
	if err := os.WriteFile(logPath, []byte("started\nfinished\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   "demo S01",
		State:     runtime.StateSucceeded,
		ExitCode:  0,
		Backend:   "local",
		Sandbox:   "none",
		LogPath:   logPath,
		Duration:  "1.5s",
		StartedAt: time.Now().UTC().Add(-2 * time.Minute),
		EndedAt:   time.Now().UTC(),
		Outputs:   []string{"/workspace/out"},
		Tags:      map[string]string{"sample": "S01"},
	}
	if err := runtime.SaveInstance(runtime.InstancePath(configDir, id), inst); err != nil {
		t.Fatal(err)
	}
	return srv, configDir
}

func TestTasksPageRequiresLogin(t *testing.T) {
	srv, _ := tasksGateway(t)
	req := httptest.NewRequest("GET", "/tasks", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Fatalf("unauthenticated /tasks = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestTasksPageListsInstances(t *testing.T) {
	srv, _ := tasksGateway(t)
	rec := srv.getAsBrowser(t, "alice", "/tasks")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"demo S01",                     // the job name
		"成功",                           // the state label
		`href="/tasks/alice-demo-s01"`, // the detail link
		"1.5s",                         // the duration
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list page is missing %q", want)
		}
	}

	// A filter that matches nothing renders the empty state rather than an error.
	rec = srv.getAsBrowser(t, "alice", "/tasks?tool=nope")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "还没有任务") {
		t.Errorf("empty filter = %d %s", rec.Code, rec.Body)
	}
}

func TestTaskPageRendersLogAndLiveClient(t *testing.T) {
	srv, _ := tasksGateway(t)
	rec := srv.getAsBrowser(t, "alice", "/tasks/alice-demo-s01")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// The tail is server-rendered, so the page is readable without JavaScript.
	if !strings.Contains(body, "started") || !strings.Contains(body, "finished") {
		t.Errorf("the log tail is missing:\n%s", body)
	}
	// The live client is progressive enhancement: it carries the id and points
	// at the SSE endpoint.
	for _, want := range []string{
		"__SRCOS_TASK__",
		"EventSource",
		"/logs?follow=1",
		"alice-demo-s01",
		"sample",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page is missing %q", want)
		}
	}
	// Artifacts are listed even when they cannot be resolved (no tool package in
	// this fixture), because the path is still the useful part.
	if !strings.Contains(body, "/workspace/out") {
		t.Errorf("the declared output is missing:\n%s", body)
	}
}

func TestTaskPageErrors(t *testing.T) {
	srv, _ := tasksGateway(t)

	rec := srv.getAsBrowser(t, "alice", "/tasks/alice-demo-nope")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "找不到这个实例") {
		t.Errorf("unknown instance = %d", rec.Code)
	}
	rec = srv.getAsBrowser(t, "alice", "/tasks/..%2F..%2Fetc")
	if rec.Code != http.StatusNotFound {
		t.Errorf("traversal = %d, want 404", rec.Code)
	}
	// Another user's instance is not found either.
	rec = srv.getAsBrowser(t, "bob", "/tasks/alice-demo-s01")
	if rec.Code != http.StatusNotFound {
		t.Errorf("another user's instance = %d, want 404", rec.Code)
	}
}
