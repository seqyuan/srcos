package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
)

// newTokenHarness wires the tool harness with an agent token store, which is
// what the gateway does at startup.
func newTokenHarness(t *testing.T) (*toolsHarness, *agenttoken.Store) {
	t.Helper()
	h := newToolsHarness(t, nil)
	store := agenttoken.New(config.AgentTokensPath(h.configDir))
	// The gateway attaches the user registry, so a token whose account is gone
	// is refused by the store itself.
	store.AttachUserCheck(h.Registry)
	h.opts.AgentTokens = store
	return h, store
}

func mintToken(t *testing.T, s *agenttoken.Store, user string, scopes ...agenttoken.Scope) string {
	t.Helper()
	_, raw, err := s.Create(agenttoken.CreateParams{User: user, Label: "test", Scopes: scopes})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return raw
}

func tokenRequest(h *Handler, method, url, token, origin string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Host = "gw:30152"
	rr := httptest.NewRecorder()
	if !h.ServeHTTP(rr, req) {
		panic("ServeHTTP did not claim " + url)
	}
	return rr
}

// The point of the surface: a program with a token reads the catalogue.
func TestAgentTokenReadsTheCatalogue(t *testing.T) {
	h, store := newTokenHarness(t)
	token := mintToken(t, store, "alice", agenttoken.ScopeRead)

	rr := tokenRequest(h.Handler, "GET", "/api/tools", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/tools = %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "demo") {
		t.Fatalf("catalogue body = %s", rr.Body)
	}

	rr = tokenRequest(h.Handler, "GET", "/api/jobs", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/jobs = %d %s", rr.Code, rr.Body)
	}
}

// The CSRF guard protects ambient cookie credentials. A bearer token is not
// ambient, and MCP clients may send their own Origin, so token requests are
// exempt — including cross-origin ones.
func TestAgentTokenIsExemptFromTheCSRFGuard(t *testing.T) {
	h, store := newTokenHarness(t)
	token := mintToken(t, store, "alice", agenttoken.ScopeRead)

	rr := tokenRequest(h.Handler, "GET", "/api/tools", token, "http://evil.example")
	if rr.Code != http.StatusOK {
		t.Fatalf("cross-origin token read = %d %s", rr.Code, rr.Body)
	}
}

// A read token is read-only: writing needs the reserved `submit` scope, which
// cannot be issued yet (ADR-019).
func TestAgentTokenCannotWrite(t *testing.T) {
	h, store := newTokenHarness(t)
	token := mintToken(t, store, "alice", agenttoken.ScopeRead)

	rr := tokenRequest(h.Handler, "POST", "/api/jobs", token, "http://evil.example")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("POST /api/jobs = %d %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "read-only") || !strings.Contains(body, "submit") {
		t.Fatalf("the refusal must explain the scope rule, got %s", body)
	}
	if strings.Contains(body, "cross-origin") {
		t.Fatalf("the CSRF guard fired for a token request: %s", body)
	}
}

// An empty bearer credential must not buy a CSRF exemption: it is not a
// credential at all, so the request falls back to the session cookie — which
// is ambient, and therefore origin-guarded.
func TestEmptyBearerDoesNotSkipTheCSRFGuard(t *testing.T) {
	h, _ := newTokenHarness(t)

	post := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/services", strings.NewReader(`{"name":"x","port":8080}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(strings.Repeat("1", 64)), false))
		req.Header.Set("Authorization", "Bearer ")
		req.Header.Set("Origin", origin)
		req.Host = "gw:30152"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if got := post("http://evil.example").Code; got != http.StatusForbidden {
		t.Fatalf("cross-origin POST with an empty bearer = %d, want 403", got)
	}
	// Same-origin, the request is still refused: an empty credential is a
	// *presented* credential, so it is rejected rather than quietly falling
	// back to the session cookie behind it.
	if got := post("http://gw:30152").Code; got != http.StatusUnauthorized {
		t.Fatalf("same-origin POST with an empty bearer = %d, want 401", got)
	}
}

func TestAgentTokenRejections(t *testing.T) {
	h, _ := newTokenHarness(t)

	// An expired token, built by hand: the plaintext has to be hashed the same
	// way Create does it, and the file is a plausible hand-edit.
	const raw = "srcos_abcdefgh." + "0123456789012345678901234567890123456789"
	sum := sha256.Sum256([]byte(raw))
	entry := fmt.Sprintf(`tokens:
  - id: abcdefgh
    user: alice
    label: old
    scopes: [read]
    created_at: 2026-01-01T00:00:00Z
    expires_at: 2026-01-02T00:00:00Z
    secret_hash: %s
`, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(config.AgentTokensPath(h.configDir), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		header string
		want   int
		note   string
		origin string
	}{
		{"unknown token", "Bearer srcos_zzzzzzzz.AAAA", 401, "unknown", ""},
		{"garbage", "Bearer hunter2", 401, "malformed", ""},
		{"wrong scheme", "Basic YWxpY2U6cHc=", 401, "Bearer", ""},
		{"expired", "Bearer " + raw, 401, "expired", ""},
		{"no token at all", "", 401, "Unauthorized", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/tools", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tc.want)
			}
			if !strings.Contains(rec.Body.String(), tc.note) {
				t.Fatalf("body = %s, want it to mention %q", rec.Body, tc.note)
			}
		})
	}
}

// Deleting an account must not leave a working credential behind, whatever
// `srcos del` did: the gateway checks the account on every agent request.
func TestAgentTokenOfDeletedUserIsRejected(t *testing.T) {
	h, store := newTokenHarness(t)
	token := mintToken(t, store, "alice", agenttoken.ScopeRead)

	if rr := tokenRequest(h.Handler, "GET", "/api/tools", token, ""); rr.Code != http.StatusOK {
		t.Fatalf("setup: token read = %d %s", rr.Code, rr.Body)
	}
	if err := os.Remove(config.UserConfigPath(h.configDir, "alice")); err != nil {
		t.Fatal(err)
	}
	h.Registry.Reload()

	rr := tokenRequest(h.Handler, "GET", "/api/tools", token, "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a deleted user's token = %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "no longer exists") {
		t.Fatalf("body = %s", rr.Body)
	}
}

// Revocation is immediate: no restart, no cache to wait out.
func TestAgentTokenRevocationIsImmediate(t *testing.T) {
	h, store := newTokenHarness(t)
	_, raw, err := store.Create(agenttoken.CreateParams{User: "alice", Scopes: []agenttoken.Scope{agenttoken.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	if rr := tokenRequest(h.Handler, "GET", "/api/tools", raw, ""); rr.Code != http.StatusOK {
		t.Fatalf("before revoke: %d %s", rr.Code, rr.Body)
	}
	tokens := store.Tokens()
	if len(tokens) != 1 {
		t.Fatalf("tokens = %+v", tokens)
	}
	if _, ok, err := store.Revoke(tokens[0].ID); err != nil || !ok {
		t.Fatalf("Revoke = %v, %v", ok, err)
	}
	if rr := tokenRequest(h.Handler, "GET", "/api/tools", raw, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("after revoke: %d %s", rr.Code, rr.Body)
	}
}

// Wiring tokens must not disturb the browser session path: sessions are
// unscoped, so they still write.
func TestSessionStillWorksAlongsideTokens(t *testing.T) {
	h, store := newTokenHarness(t)
	mintToken(t, store, "alice", agenttoken.ScopeRead)

	cookie := auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(strings.Repeat("1", 64)), false)
	req := httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session read = %d %s", rec.Code, rec.Body)
	}

	// A session may submit (subject to the other checks, hence the same-origin
	// header the existing tests use).
	post := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"tool":"demo","params":{"ref":"/data/ref"}}`))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Cookie", cookie)
	post.Header.Set("Origin", "http://gw:30152")
	post.Host = "gw:30152"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, post)
	if rec.Code != http.StatusCreated {
		t.Fatalf("session submit = %d %s", rec.Code, rec.Body)
	}
}

// A dead token stream must not be able to write even via a state-changing verb
// the scope gate does not know about: the gate is "anything that is not a read".
func TestScopeGateCoversEveryWriteMethod(t *testing.T) {
	h, store := newTokenHarness(t)
	token := mintToken(t, store, "alice", agenttoken.ScopeRead)

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rr := tokenRequest(h.Handler, method, "/api/services/x", token, "http://gw:30152")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s = %d %s, want 403", method, rr.Code, rr.Body)
		}
	}
}
