package server

import (
	"encoding/json"
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

// mcpGateway builds a gateway with a tool package, a grant, and one agent
// token, so the MCP endpoint can be exercised through the real mux.
func mcpGateway(t *testing.T) (*Server, string) {
	t.Helper()
	configDir := t.TempDir()
	if err := os.MkdirAll(config.UsersDir(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	userCfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
	if err := os.WriteFile(config.UserConfigPath(configDir, "alice"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.GrantsPath(configDir),
		[]byte("grants:\n  - tool: demo\n    public: true\n"), 0o600); err != nil {
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
	}, configDir, Options{ToolsDir: toolsDir, Version: "test"})

	_, raw, err := agenttoken.New(config.AgentTokensPath(configDir)).Create(agenttoken.CreateParams{
		User: "alice", Label: "mcp test",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return srv, raw
}

func mcpPost(t *testing.T, srv *Server, body, authorization string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, rec.Body)
		}
	}
	return rec, decoded
}

// The gateway end to end: initialize, list the nine read-only tools, and call
// one, all through the real mux (so the reserved /mcp route is exercised too).
func TestMCPThroughTheGateway(t *testing.T) {
	srv, token := mcpGateway(t)

	rec, body := mcpPost(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	result, _ := body["result"].(map[string]any)
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != "srcos" || info["version"] != "test" {
		t.Fatalf("serverInfo = %v", info)
	}

	rec, body = mcpPost(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list: %d %s", rec.Code, rec.Body)
	}
	result, _ = body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 9 {
		t.Fatalf("expected the nine read-only tools, got %d", len(tools))
	}

	rec, body = mcpPost(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"srcos_list_tools","arguments":{}}}`, "Bearer "+token)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
	}
	result, _ = body["result"].(map[string]any)
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v", result)
	}
	text, _ := content[0].(map[string]any)
	if !strings.Contains(text["text"].(string), `"id": "demo"`) {
		t.Fatalf("the catalogue should contain demo: %v", text)
	}

	// A session cookie is not a credential for MCP: it is the program surface.
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`))
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(strings.Repeat("1", 64)), false))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a session must not authenticate MCP: %d %s", rec.Code, rec.Body)
	}

	// And the gateway's other routes are untouched by the new one.
	req = httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(strings.Repeat("1", 64)), false))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/tools: %d", rec.Code)
	}
}

// Revoking a token must lock an agent out immediately — the same rule the REST
// API follows, checked here through the MCP route.
func TestMCPRevocationIsImmediate(t *testing.T) {
	srv, token := mcpGateway(t)
	if rec, _ := mcpPost(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "Bearer "+token); rec.Code != http.StatusOK {
		t.Fatalf("before revoke: %d", rec.Code)
	}

	store := agenttoken.New(config.AgentTokensPath(config.DirOf(srv.registry)))
	tokens := store.Tokens()
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	tokens = store.Tokens()
	if len(tokens) != 1 {
		t.Fatalf("tokens = %+v", tokens)
	}
	if _, ok, err := store.Revoke(tokens[0].ID); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}

	rec, _ := mcpPost(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, "Bearer "+token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: %d %s", rec.Code, rec.Body)
	}
}
