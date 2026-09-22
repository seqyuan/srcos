package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
)

// adminHarness wires the management surface: two users, one admin (root), one
// tool package, one running instance, a policy file, and no supervisor (the
// tests that need to stop something bring their own).
type adminHarness struct {
	*Handler
	configDir  string
	toolsDir   string
	policy     *grant.Policy
	policyPath string
}

func newAdminHarness(t *testing.T) *adminHarness {
	t.Helper()
	configDir := t.TempDir()
	for _, user := range []string{"root", "alice"} {
		if err := os.MkdirAll(config.UsersDir(configDir), 0o700); err != nil {
			t.Fatal(err)
		}
		cfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
		if err := os.WriteFile(config.UserConfigPath(configDir, user), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	toolsDir := t.TempDir()
	dir := filepath.Join(toolsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schemaVersion: 1\nid: demo\nversion: 0.1.0\nname: Demo\nkind: task\nbackend: local\n" +
		"entry: work.sh\ninterface:\n  inputs:\n    - {name: label, type: string}\n" +
		"resources: {cpu: 1, memory: \"512Mi\", walltime: \"0:10:00\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	policyPath := filepath.Join(configDir, "grants.yaml")
	policy, err := grant.New(map[string][]string{"bio": {"alice"}}, []string{"root"}, []grant.Grant{
		{Tool: "demo", Groups: []string{"bio"}, Quota: grant.Quota{MaxCPU: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.Save(policyPath, policy); err != nil {
		t.Fatal(err)
	}

	registry := config.NewUserRegistry(configDir)
	registry.Reload()
	h := NewHandlerWithOptions(registry, "testsecret", Options{
		ConfigDir:  configDir,
		ToolsDir:   toolsDir,
		Policy:     policy,
		PolicyPath: policyPath,
	})
	return &adminHarness{Handler: h, configDir: configDir, toolsDir: toolsDir, policy: policy, policyPath: policyPath}
}

func (h *adminHarness) asAdmin(method, url, body string) *httptest.ResponseRecorder {
	return h.as(method, url, body, "root")
}

func (h *adminHarness) as(method, url, body, user string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, user, auth.SessionRev(strings.Repeat("1", 64)), false))
	req.Header.Set("Origin", "http://gw:30152")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, req) {
		panic("ServeHTTP declined " + url)
	}
	return rec
}

// ─────────────────────────────────────────────────────────────────────────
// 权限：管理面只属于管理员
// ─────────────────────────────────────────────────────────────────────────

func TestAdminSurfaceIsAdminOnly(t *testing.T) {
	h := newAdminHarness(t)

	for _, tc := range []struct{ method, url, body string }{
		{"GET", "/api/admin/instances", ""},
		{"GET", "/api/admin/tools", ""},
		{"GET", "/api/admin/policy", ""},
		{"GET", "/api/admin/groups", ""},
		{"PUT", "/api/admin/grants/demo", `{"public":true}`},
		{"DELETE", "/api/admin/grants/demo", ""},
		{"PUT", "/api/admin/admins", `{"admins":["alice"]}`},
		{"POST", "/api/admin/instances/alice-demo-x/stop", ""},
	} {
		// Not logged in: 401 (a session is required before anything else).
		req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
		req.Host = "gw:30152"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session = %d, want 401", tc.method, tc.url, rec.Code)
		}

		// Logged in, not an admin: 403 — and the policy must be untouched.
		rec = h.as(tc.method, tc.url, tc.body, "alice")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin = %d %s, want 403", tc.method, tc.url, rec.Code, rec.Body)
		}
		if g, ok := h.policy.Grant("demo"); !ok || len(g.Groups) != 1 || g.Public {
			t.Errorf("%s %s changed the policy: %+v", tc.method, tc.url, g)
		}
	}
}

// The admin surface is not a way around the CSRF guard: a cross-site form must
// not be able to grant somebody access.
func TestAdminWritesRequireSameOrigin(t *testing.T) {
	h := newAdminHarness(t)
	req := httptest.NewRequest("PUT", "/api/admin/grants/demo", strings.NewReader(`{"public":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "root", auth.SessionRev(strings.Repeat("1", 64)), false))
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin admin write = %d %s", rec.Code, rec.Body)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 工具与授权
// ─────────────────────────────────────────────────────────────────────────

func TestAdminToolsShowsTheAuthorizationState(t *testing.T) {
	h := newAdminHarness(t)
	rec := h.asAdmin("GET", "/api/admin/tools", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"id":"demo"`, `"reachable":true`, `"groups":["bio"]`, `"maxCpu":2`} {
		if !strings.Contains(body, want) {
			t.Errorf("tools view is missing %s\n%s", want, body)
		}
	}
}

// Grant changes take effect on the live policy *and* land in the file: a console
// that says "saved" while requests still see the old answer would be worse than
// no console.
func TestAdminSetGrantTakesEffectAndPersists(t *testing.T) {
	h := newAdminHarness(t)
	if h.policy.Allowed("bob", "demo") {
		t.Fatal("setup: bob is not granted anything")
	}

	rec := h.asAdmin("PUT", "/api/admin/grants/demo", `{"users":["bob"],"maxCpu":8,"maxMemory":"4Gi"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if !h.policy.Allowed("bob", "demo") {
		t.Fatal("the change must be live immediately (no reload, no restart)")
	}
	if q := h.policy.QuotaFor("bob", "demo"); q.MaxCPU != 8 || q.MaxMemory != "4Gi" {
		t.Fatalf("quota = %+v", q)
	}
	// The groups list is replaced by the body, not merged: `grant set` semantics.
	if h.policy.Allowed("alice", "demo") {
		t.Fatal("replacing the grant must drop the old group")
	}

	onDisk, err := grant.Load(h.policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.Allowed("bob", "demo") {
		t.Fatal("the change must be persisted")
	}
	if onDisk.Allowed("alice", "demo") {
		t.Fatal("the file still has the old grant")
	}
}

func TestAdminGrantValidation(t *testing.T) {
	h := newAdminHarness(t)

	cases := map[string]struct {
		tool string
		body string
		want int
	}{
		"unknown group": {"demo", `{"groups":["nope"]}`, 400},
		"admits nobody": {"demo", `{"users":[]}`, 400},
		"bad json":      {"demo", `{`, 400},
		"unknown tool is allowed (grants are just statements)": {"ghost", `{"public":true}`, 200},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := h.asAdmin("PUT", "/api/admin/grants/"+tc.tool, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}

	// Removing a grant denies everyone but admins.
	if rec := h.asAdmin("DELETE", "/api/admin/grants/demo", ""); rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if h.policy.Allowed("alice", "demo") {
		t.Fatal("a removed grant must deny")
	}
	if !h.policy.Allowed("root", "demo") {
		t.Fatal("admins bypass grants")
	}
	if rec := h.asAdmin("DELETE", "/api/admin/grants/demo", ""); rec.Code != 404 {
		t.Fatalf("deleting twice: %d", rec.Code)
	}
}

func TestAdminGroupsAndAdmins(t *testing.T) {
	h := newAdminHarness(t)

	rec := h.asAdmin("GET", "/api/admin/groups", "")
	if !strings.Contains(rec.Body.String(), `"bio":["alice"]`) {
		t.Fatalf("groups = %s", rec.Body)
	}

	// Replacing a group's members is the policy's own operation.
	if rec := h.asAdmin("PUT", "/api/admin/groups", `{"name":"bio","users":["alice","bob"]}`); rec.Code != 200 {
		t.Fatalf("set group: %d %s", rec.Code, rec.Body)
	}
	if !h.policy.Allowed("bob", "demo") {
		t.Fatal("a new member must be admitted by the existing grant")
	}

	// Deleting a group a grant still names would silently deny those users.
	rec = h.asAdmin("PUT", "/api/admin/groups", `{"name":"bio","users":[]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("removing a group that grants name = %d %s", rec.Code, rec.Body)
	}

	// Admins: the last one may not be removed (that is a lockout).
	if rec := h.asAdmin("PUT", "/api/admin/admins", `{"admins":[]}`); rec.Code != 400 {
		t.Fatalf("emptying the admin list = %d %s", rec.Code, rec.Body)
	}
	if rec := h.asAdmin("PUT", "/api/admin/admins", `{"admins":["root","alice"]}`); rec.Code != 200 {
		t.Fatalf("adding an admin: %d %s", rec.Code, rec.Body)
	}
	if !h.policy.IsAdmin("alice") {
		t.Fatal("the new admin must be an admin")
	}
	// The new admin can use the surface; the old one still can.
	if rec := h.as("GET", "/api/admin/groups", "", "alice"); rec.Code != 200 {
		t.Fatalf("a newly promoted admin = %d", rec.Code)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 实例总览
// ─────────────────────────────────────────────────────────────────────────

func TestAdminInstancesAcrossUsers(t *testing.T) {
	h := newAdminHarness(t)
	// Two users' instances (alice's is running, so the overview tries to sample
	// it; without a runner it simply shows no usage).
	for _, spec := range []struct {
		user, tool, jobID, state string
	}{
		{"alice", "demo", "run-1", "succeeded"},
		{"root", "demo", "run-2", "running"},
	} {
		paths := runtime.PathsFor(h.configDir, spec.user, spec.tool, spec.jobID)
		if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
			t.Fatal(err)
		}
		inst := &runtime.Instance{
			ID: runtime.InstanceID(spec.user, spec.tool, spec.jobID), User: spec.user, Tool: spec.tool,
			Kind: "task", JobName: spec.jobID, State: runtime.State(spec.state), Backend: "local",
			LogPath: paths.LogPath, StartedAt: time.Now().UTC(),
		}
		if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
			t.Fatal(err)
		}
	}

	rec := h.asAdmin("GET", "/api/admin/instances", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"owner":"alice"`, `"owner":"root"`, `"id":"alice-demo-run-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview is missing %s\n%s", want, body)
		}
	}
	// A per-user reader would not see this; the admin view must.
	if !strings.Contains(body, `"kind":"task"`) {
		t.Fatalf("instance shape changed: %s", body)
	}

	// Filters exist because an operator hunting a runaway service wants them.
	if rec := h.asAdmin("GET", "/api/admin/instances?kind=service", ""); strings.Contains(rec.Body.String(), "alice-demo-run-1") {
		t.Fatalf("kind filter leaked tasks: %s", rec.Body)
	}
}

func TestAdminLogsAndStop(t *testing.T) {
	h := newAdminHarness(t)
	paths := runtime.PathsFor(h.configDir, "alice", "demo", "run-1")
	if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LogPath, []byte("first\nsecond\nthird\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID: runtime.InstanceID("alice", "demo", "run-1"), User: "alice", Tool: "demo",
		Kind: "task", State: runtime.StateRunning, Backend: "local",
		LogPath: paths.LogPath, StartedAt: time.Now().UTC(),
	}
	if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
		t.Fatal(err)
	}

	rec := h.asAdmin("GET", "/api/admin/instances/"+inst.ID+"/logs?tail=2", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "third") || strings.Contains(rec.Body.String(), "first") {
		t.Fatalf("logs = %d %s", rec.Code, rec.Body)
	}
	if rec := h.asAdmin("GET", "/api/admin/instances/nope/logs", ""); rec.Code != 404 {
		t.Fatalf("unknown instance: %d", rec.Code)
	}

	// Without a supervisor there is nothing to stop with: say so rather than
	// pretending the instance stopped.
	rec = h.asAdmin("POST", "/api/admin/instances/"+inst.ID+"/stop", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("stop without a supervisor = %d %s", rec.Code, rec.Body)
	}
}
