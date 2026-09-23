package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/agenttoken"
)

// sessionJSON posts a JSON body as the browser session (Origin set, as a real
// browser would send it).
func sessionJSON(t *testing.T, h *toolsHarness, method, url, cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "http://gw:30152")
	req.Header.Set("Content-Type", "application/json")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, req) {
		t.Fatalf("ServeHTTP declined %s %s", method, url)
	}
	return rec
}

func TestTokenSelfServiceRoundTrip(t *testing.T) {
	h, _ := newTokenHarness(t)

	// Nothing yet.
	rec := h.get(t, "/api/tokens")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tokens"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "srcos_") {
		t.Errorf("the listing must never contain a plaintext token: %s", rec.Body)
	}

	// Create one with the submit scope and an allowlist.
	body := `{"label":"ci","scopes":["submit"],"tools":["demo"],"expires":"90d"}`
	rec = sessionJSON(t, h, "POST", "/api/tokens", h.cookie, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var created struct {
		Token struct {
			ID     string   `json:"id"`
			Scopes []string `json:"scopes"`
			Tools  []string `json:"tools"`
			Status string   `json:"status"`
		} `json:"token"`
		Plaintext string `json:"plaintext"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Plaintext, "srcos_") {
		t.Fatalf("no plaintext returned: %s", rec.Body)
	}
	// submit implies read.
	if strings.Join(created.Token.Scopes, ",") != "read,submit" || created.Token.Status != "active" {
		t.Fatalf("token = %+v", created.Token)
	}
	if len(created.Token.Tools) != 1 || created.Token.Tools[0] != "demo" {
		t.Fatalf("tools = %v", created.Token.Tools)
	}

	// The credential works immediately, and the listing shows it once.
	rec = tokenRequest(h.Handler, "GET", "/api/tools", created.Plaintext, "")
	if rec.Code != 200 {
		t.Fatalf("the new token was refused: %d %s", rec.Code, rec.Body)
	}
	rec = h.get(t, "/api/tokens")
	if !strings.Contains(rec.Body.String(), created.Token.ID) || strings.Contains(rec.Body.String(), created.Plaintext) {
		t.Fatalf("listing = %s", rec.Body)
	}

	// Revoke, and it stops working at once.
	rec = sessionJSON(t, h, "DELETE", "/api/tokens/"+created.Token.ID, h.cookie, "")
	if rec.Code != 200 {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	rec = tokenRequest(h.Handler, "GET", "/api/tools", created.Plaintext, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token still works: %d", rec.Code)
	}
}

// A program must not be able to manage credentials: a leaked submit token could
// otherwise mint itself a replacement that outlives its own revocation.
func TestTokenManagementIsSessionOnly(t *testing.T) {
	h, store := newTokenHarness(t)
	submit := mintSubmitToken(t, h, store)

	for _, c := range []struct {
		method, url, body string
	}{
		{"GET", "/api/tokens", ""},
		{"POST", "/api/tokens", `{"label":"x"}`},
		{"DELETE", "/api/tokens/abc12345", ""},
	} {
		rec := tokenJSON(h.Handler, c.method, c.url, submit, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with an agent token = %d %s, want 403", c.method, c.url, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), "browser session") {
			t.Errorf("%s %s: message should explain why: %s", c.method, c.url, rec.Body)
		}
	}
}

func TestTokenListIsScopedToTheCaller(t *testing.T) {
	h, store := newTokenHarness(t)
	// A token that belongs to somebody else.
	if _, _, err := store.Create(agenttoken.CreateParams{User: "bob", Label: "bob's"}); err != nil {
		t.Fatal(err)
	}
	mintToken(t, store, "alice", agenttoken.ScopeRead)
	var myID string
	for _, tok := range store.Tokens() {
		if tok.User == "alice" {
			myID = tok.ID
		}
	}

	rec := h.get(t, "/api/tokens")
	if strings.Contains(rec.Body.String(), "bob") {
		t.Fatalf("another user's token leaked into the listing: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), myID) {
		t.Fatalf("my own token is missing: %s", rec.Body)
	}

	// And revoking someone else's is "not found", not "forbidden": a caller must
	// not be able to probe which token ids exist.
	var bobID string
	for _, tok := range store.Tokens() {
		if tok.User == "bob" {
			bobID = tok.ID
		}
	}
	rec = sessionJSON(t, h, "DELETE", "/api/tokens/"+bobID, h.cookie, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoking another user's token = %d %s", rec.Code, rec.Body)
	}
	if len(store.Tokens()) != 2 {
		t.Fatal("the other user's token was removed")
	}
}

func TestTokenAllowlistMustBeSomethingTheUserCanUse(t *testing.T) {
	h, _ := newTokenHarness(t)

	rec := sessionJSON(t, h, "POST", "/api/tokens", h.cookie, `{"label":"x","scopes":["submit"],"tools":["nope"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not authorized") {
		t.Fatalf("ungranted tool in the allowlist = %d %s", rec.Code, rec.Body)
	}
	// An unknown scope is refused by the store's own rules.
	rec = sessionJSON(t, h, "POST", "/api/tokens", h.cookie, `{"label":"x","scopes":["admin"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown scope = %d %s", rec.Code, rec.Body)
	}
	// A bad expiry too.
	rec = sessionJSON(t, h, "POST", "/api/tokens", h.cookie, `{"label":"x","expires":"soon"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad expiry = %d %s", rec.Code, rec.Body)
	}
	// read-only, no allowlist: the default shape.
	rec = sessionJSON(t, h, "POST", "/api/tokens", h.cookie, `{"label":"plain","scopes":["read"]}`)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"read"`) {
		t.Fatalf("read token = %d %s", rec.Code, rec.Body)
	}
}

func TestTokenCreationIsCapped(t *testing.T) {
	h, store := newTokenHarness(t)
	for i := 0; i < 20; i++ {
		if _, _, err := store.Create(agenttoken.CreateParams{User: "alice", Label: "bulk"}); err != nil {
			t.Fatal(err)
		}
	}
	rec := sessionJSON(t, h, "POST", "/api/tokens", h.cookie, `{"label":"one too many"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "revoke one") {
		t.Fatalf("the cap did not hold: %d %s", rec.Code, rec.Body)
	}
}
