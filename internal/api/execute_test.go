package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/agenttoken"
)

// tokenJSON posts a JSON body with a bearer credential. The bearer path is
// exempt from the Origin guard (a token is not an ambient credential), so no
// Origin header is needed.
func tokenJSON(h *Handler, method, url, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, req) {
		panic("ServeHTTP did not claim " + url)
	}
	return rec
}

// mintSubmitToken creates a submit-scoped credential, optionally narrowed to a
// tool allowlist.
func mintSubmitToken(t *testing.T, h *toolsHarness, s *agenttoken.Store, tools ...string) string {
	t.Helper()
	_, raw, err := s.Create(agenttoken.CreateParams{
		User:        "alice",
		Label:       "ci",
		Scopes:      []agenttoken.Scope{agenttoken.ScopeSubmit},
		SubmitTools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// submitBody is a valid submission for the harness's demo tool.
const submitBody = `{"tool":"demo","params":{"ref":"/data"}}`

func TestSubmitRequiresTheSubmitScope(t *testing.T) {
	h, store := newTokenHarness(t)

	// A read-only token is refused by the scope gate (the same rule the read
	// surface has always had).
	read := mintToken(t, store, "alice", agenttoken.ScopeRead)
	rec := tokenJSON(h.Handler, "POST", "/api/jobs", read, submitBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only token submit = %d %s", rec.Code, rec.Body)
	}

	// A submit token may submit...
	submit := mintSubmitToken(t, h, store)
	rec = tokenJSON(h.Handler, "POST", "/api/jobs", submit, submitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit token = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"jobId"`) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestSubmitHonoursTheToolAllowlistOverHTTP(t *testing.T) {
	h, store := newTokenHarness(t)

	// The allowlist names a tool other than the one being submitted.
	narrowed := mintSubmitToken(t, h, store, "other")
	rec := tokenJSON(h.Handler, "POST", "/api/jobs", narrowed, submitBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("allowlist miss = %d %s", rec.Code, rec.Body)
	}

	// The allowlisted tool works (the harness has no "other" tool, so this
	// asserts the allowlist passed and the *tool lookup* is what failed).
	allowed := mintSubmitToken(t, h, store, "demo")
	rec = tokenJSON(h.Handler, "POST", "/api/jobs", allowed, submitBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("allowlisted tool = %d %s", rec.Code, rec.Body)
	}
}

func TestCancelEndpointGuards(t *testing.T) {
	h, store := newTokenHarness(t)

	// A read-only token cannot cancel (the scope gate).
	read := mintToken(t, store, "alice", agenttoken.ScopeRead)
	rec := tokenRequest(h.Handler, "POST", "/api/jobs/alice-demo-x/cancel", read, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("read-only cancel = %d %s", rec.Code, rec.Body)
	}

	// A submit token asking for an instance it does not have gets 404.
	submit := mintSubmitToken(t, h, store, "demo")
	rec = tokenRequest(h.Handler, "POST", "/api/jobs/alice-demo-nope/cancel", submit, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown instance = %d %s", rec.Code, rec.Body)
	}

	// The shape is fixed: <id>/cancel only.
	rec = tokenRequest(h.Handler, "POST", "/api/jobs/alice-demo-x/other", submit, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("wrong shape = %d", rec.Code)
	}
	// A traversal never reaches the store.
	rec = tokenRequest(h.Handler, "POST", "/api/jobs/..%2F..%2Fetc/cancel", submit, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal = %d", rec.Code)
	}
}

func TestRunFlowEndpointWithoutFlowsDir(t *testing.T) {
	h, store := newTokenHarness(t)
	submit := mintSubmitToken(t, h, store)
	rec := tokenJSON(h.Handler, "POST", "/api/flows/pipe/run", submit, `{"samples":"word\nhello\n"}`)
	// The harness has no flow directory, so this deployment cannot run flows.
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("run_flow without a flows dir = %d %s", rec.Code, rec.Body)
	}
}
