package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/accessrequest"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
)

// requestHarness wires the request surface: root is the admin, alice is a plain
// user who has "open" but must ask for "ask".
type requestHarness struct {
	*Handler
	configDir string
	policy    *grant.Policy
}

func newRequestHarness(t *testing.T) *requestHarness {
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
	for _, id := range []string{"open", "ask", "closed"} {
		dir := filepath.Join(toolsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := "schemaVersion: 1\nid: " + id + "\nversion: 0.1.0\nname: " + id +
			"\nkind: task\nbackend: local\nentry: work.sh\n" +
			"resources: {cpu: 1, memory: \"512Mi\", walltime: \"0:10:00\"}\n"
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	policyPath := filepath.Join(configDir, "grants.yaml")
	// open: alice has it (via public). ask: requestable, alice does not have it.
	// closed: exists, nobody has it, not requestable.
	policy, err := grant.New(nil, []string{"root"}, []grant.Grant{
		{Tool: "open", Public: true},
		{Tool: "ask", Users: []string{"carol"}, Requestable: true},
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
		Audit:      audit.New(config.DataDir(configDir)),
	})
	return &requestHarness{Handler: h, configDir: configDir, policy: policy}
}

func (h *requestHarness) do(t *testing.T, method, url, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, user, auth.SessionRev(strings.Repeat("1", 64)), false))
	req.Header.Set("Origin", "http://gw:30152")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, req) {
		t.Fatalf("ServeHTTP declined %s %s", method, url)
	}
	return rec
}

func requestID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Request accessrequest.Request `json:"request"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if out.Request.ID == "" {
		t.Fatalf("no request id in %s", rec.Body.String())
	}
	return out.Request.ID
}

func TestRequestLifecycle(t *testing.T) {
	h := newRequestHarness(t)

	// alice asks for the requestable tool.
	rec := h.do(t, "POST", "/api/requests", `{"tool":"ask","reason":"要跑一次"}`, "alice")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	id := requestID(t, rec)

	// Asking again is idempotent, not a second file.
	rec = h.do(t, "POST", "/api/requests", `{"tool":"ask","reason":"again"}`, "alice")
	if rec.Code != http.StatusOK || requestID(t, rec) != id {
		t.Fatalf("second ask should return the pending request: %d %s", rec.Code, rec.Body.String())
	}

	// A non-requestable tool is refused.
	if rec := h.do(t, "POST", "/api/requests", `{"tool":"closed"}`, "alice"); rec.Code != http.StatusForbidden {
		t.Fatalf("closed tool = %d, want 403", rec.Code)
	}
	// A tool alice already has needs no request.
	if rec := h.do(t, "POST", "/api/requests", `{"tool":"open"}`, "alice"); rec.Code != http.StatusBadRequest {
		t.Fatalf("already-granted tool = %d, want 400", rec.Code)
	}
	// An unknown tool is 404.
	if rec := h.do(t, "POST", "/api/requests", `{"tool":"nope"}`, "alice"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tool = %d, want 404", rec.Code)
	}

	// The admin sees it pending.
	rec = h.do(t, "GET", "/api/admin/requests?state=pending", "", "root")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending":1`) {
		t.Fatalf("admin list = %d %s", rec.Code, rec.Body.String())
	}
	// A non-admin cannot.
	if rec := h.do(t, "GET", "/api/admin/requests", "", "alice"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin list = %d, want 403", rec.Code)
	}

	// Approve: the grant is written and the request decided.
	if rec := h.do(t, "POST", "/api/admin/requests/"+id+"/approve", "", "root"); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}
	if !h.policy.Allowed("alice", "ask") {
		t.Fatal("approval must grant access")
	}
	if got, ok := h.policy.Grant("ask"); !ok || len(got.Users) == 0 {
		t.Fatalf("grant not written: %+v", got)
	}

	// Deciding twice is a conflict, not a silent overwrite.
	if rec := h.do(t, "POST", "/api/admin/requests/"+id+"/deny", `{"note":"too late"}`, "root"); rec.Code != http.StatusConflict {
		t.Fatalf("second decision = %d, want 409", rec.Code)
	}

	// alice sees the approval.
	rec = h.do(t, "GET", "/api/requests", "", "alice")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"state":"approved"`) {
		t.Fatalf("own list = %d %s", rec.Code, rec.Body.String())
	}

	// Both acts are audited.
	for _, action := range []string{"request.create", "request.approve"} {
		events, err := audit.Query(config.DataDir(h.configDir), audit.Filter{Action: action})
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatalf("%s events = %d, want 1", action, len(events))
		}
	}
}

func TestRequestDeny(t *testing.T) {
	h := newRequestHarness(t)
	rec := h.do(t, "POST", "/api/requests", `{"tool":"ask"}`, "alice")
	id := requestID(t, rec)

	if rec := h.do(t, "POST", "/api/admin/requests/"+id+"/deny", `{"note":"not now"}`, "root"); rec.Code != http.StatusOK {
		t.Fatalf("deny = %d: %s", rec.Code, rec.Body.String())
	}
	if h.policy.Allowed("alice", "ask") {
		t.Fatal("a denial must not grant anything")
	}
	got, err := accessrequest.Get(config.DataDir(h.configDir), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != accessrequest.Denied || got.Note != "not now" {
		t.Fatalf("denied request = %+v", got)
	}
	events, err := audit.Query(config.DataDir(h.configDir), audit.Filter{Action: "request.deny"})
	if err != nil || len(events) != 1 {
		t.Fatalf("request.deny events = %d (%v)", len(events), err)
	}
}

func TestUnknownRequestDecisionIs404(t *testing.T) {
	h := newRequestHarness(t)
	if rec := h.do(t, "POST", "/api/admin/requests/req-nope/approve", "", "root"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown request = %d, want 404", rec.Code)
	}
}
