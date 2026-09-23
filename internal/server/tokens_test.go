package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
)

// tokensGateway is proxyGateway plus a tools directory and one minted token, so
// the page has both a row and something to offer in the allowlist.
func tokensGateway(t *testing.T) *Server {
	t.Helper()
	srv, configDir := proxyGateway(t)
	toolsDir := t.TempDir()
	toolDir := toolsDir + "/demo"
	if err := writeToolPackage(toolDir, "demo"); err != nil {
		t.Fatal(err)
	}
	srv.toolsDir = toolsDir
	// The page offers the user's visible tools in the allowlist, so this
	// deployment needs a policy that lets alice see one.
	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "*", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	srv.grants = policy

	store := agenttoken.New(config.AgentTokensPath(configDir))
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Create(agenttoken.CreateParams{User: "alice", Label: "alice-agent-one"}); err != nil {
		t.Fatal(err)
	}
	srv.agentTokens = store
	return srv
}

func TestTokensPageRequiresLogin(t *testing.T) {
	srv := tokensGateway(t)
	req := httptest.NewRequest("GET", "/tokens", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Fatalf("unauthenticated /tokens = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestTokensPageListsTheUsersOwnTokens(t *testing.T) {
	srv := tokensGateway(t)
	rec := srv.getAsBrowser(t, "alice", "/tokens")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Agent token",
		"alice-agent-one",  // the label of alice's token
		"明文只出现一次",          // the one-shot warning
		"demo",             // the tool allowlist offers what she may use
		"__SRCOS_TOKENS__", // the boot object the create form uses
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Another user's page must not show alice's token.
	rec = srv.getAsBrowser(t, "bob", "/tokens")
	if strings.Contains(rec.Body.String(), "alice-agent-one") {
		t.Error("alice's token leaked into bob's page")
	}
}

func TestTokensPageNeverRendersAPlaintext(t *testing.T) {
	srv := tokensGateway(t)
	rec := srv.getAsBrowser(t, "alice", "/tokens")
	if strings.Contains(rec.Body.String(), "srcos_") &&
		!strings.Contains(rec.Body.String(), "srcos_&lt;") { // the doc example is fine
		t.Errorf("a plaintext token (or its prefix) appears in the page:\n%s", rec.Body)
	}
}

// writeToolPackage writes the minimal tool package the page needs to offer an
// allowlist entry.
func writeToolPackage(dir, id string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	manifest := "schemaVersion: 1\nid: " + id + "\nversion: 0.1.0\nname: " + id +
		"\nkind: task\nbackend: local\nsandbox: none\nentry: work.sh\n" +
		"interface:\n  inputs:\n    - {name: word, type: string, required: true}\n" +
		"resources: {cpu: 1, memory: \"1Gi\", walltime: \"0:10:00\"}\n"
	if err := os.WriteFile(dir+"/tool.yaml", []byte(manifest), 0o644); err != nil {
		return err
	}
	return os.WriteFile(dir+"/work.sh", []byte("#!/bin/sh\nexit 0\n"), 0o755)
}
