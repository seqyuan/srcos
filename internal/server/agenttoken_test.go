package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
)

// The gateway end to end: a token minted by `srcos token create` reads the
// tool catalogue over HTTP, and the same token cannot write.
//
// This is the wiring test for server.NewWithOptions — the api package's tests
// prove the rules, this one proves the gateway actually attaches the store.
func TestAgentTokenThroughTheGateway(t *testing.T) {
	configDir := t.TempDir()
	usersDir := config.UsersDir(configDir)
	if err := os.MkdirAll(usersDir, 0o700); err != nil {
		t.Fatal(err)
	}
	userCfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
	if err := os.WriteFile(config.UserConfigPath(configDir, "alice"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// A tool that alice may see, so the catalogue is not empty for a reason
	// other than the token.
	grants := "grants:\n  - tool: demo\n    public: true\n"
	if err := os.WriteFile(config.GrantsPath(configDir), []byte(grants), 0o600); err != nil {
		t.Fatal(err)
	}

	toolsDir := t.TempDir()
	toolDir := filepath.Join(toolsDir, "demo")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schemaVersion: 1\nid: demo\nversion: 0.1.0\nname: Demo\nkind: task\n" +
		"backend: local\nentry: work.sh\ninterface:\n  inputs:\n    - {name: label, type: string}\n" +
		"resources: {cpu: 1, memory: \"512Mi\", walltime: \"0:10:00\"}\n"
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	srv := NewWithOptions(&config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 30152},
		Auth:   config.AuthState{SessionSecret: "testsecret", SessionTTL: 86400},
	}, configDir, Options{ToolsDir: toolsDir})

	_, raw, err := agenttoken.New(config.AgentTokensPath(configDir)).Create(agenttoken.CreateParams{
		User: "alice", Label: "gateway test",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/tools = %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), `"id":"demo"`) {
		t.Fatalf("catalogue = %s", rr.Body)
	}

	post := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"tool":"demo"}`))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer "+raw)
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /api/jobs with a read token = %d %s", rr.Code, rr.Body)
	}

	// A browser session is unaffected by the token surface.
	session := httptest.NewRequest("GET", "/api/tools", nil)
	session.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(strings.Repeat("1", 64)), false))
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, session)
	if rr.Code != http.StatusOK {
		t.Fatalf("session GET /api/tools = %d %s", rr.Code, rr.Body)
	}
}
