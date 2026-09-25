package agenttoken

import (
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-tokens.yaml")
	return New(path), path
}

func mustCreate(t *testing.T, s *Store, p CreateParams) (Token, string) {
	t.Helper()
	rec, raw, err := s.Create(p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return rec, raw
}

// lookup is a UserLookup over a fixed set of names.
type lookup map[string]bool

func (l lookup) GetUser(username string) *config.UserRecord {
	if l[username] {
		return &config.UserRecord{Username: username}
	}
	return nil
}

// A credential must not outlive its account: `srcos del` revokes a user's
// tokens, but a restored backup or a recreated name must not resurrect access.
func TestTokenOfADeletedUserIsRefused(t *testing.T) {
	s, _ := newTestStore(t)
	_, raw := mustCreate(t, s, CreateParams{User: "alice"})
	s.AttachUserCheck(lookup{"alice": true})

	if _, err := s.verifyAt(raw, time.Now()); err != nil {
		t.Fatalf("the account exists, so the token must work: %v", err)
	}

	s.AttachUserCheck(lookup{}) // alice is gone
	_, err := s.verifyAt(raw, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("error = %v, want a deleted-user rejection", err)
	}
}

// The whole point of the surface: a program presents a bearer token and gets
// an identity.
func TestCreateThenAuthenticate(t *testing.T) {
	s, path := newTestStore(t)
	rec, raw := mustCreate(t, s, CreateParams{User: "alice", Label: "annovibe"})

	if !strings.HasPrefix(raw, TokenPrefix) {
		t.Fatalf("token %q lacks the %q prefix", raw, TokenPrefix)
	}
	if !strings.HasPrefix(raw, TokenPrefix+rec.ID+".") {
		t.Fatalf("token %q does not embed its id %q", raw, rec.ID)
	}
	if ScopeSet(rec.Scopes).String() != "read" {
		t.Fatalf("default scopes = %v, want read", rec.Scopes)
	}
	if !rec.ExpiresAt.IsZero() {
		t.Fatalf("an unset expiry must stay unset, got %v", rec.ExpiresAt)
	}

	req := httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	ident, err := s.Authenticate(req)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if ident.User != "alice" || !ident.Agent || ident.TokenID != rec.ID {
		t.Fatalf("identity = %+v", ident)
	}
	if !ident.Has(ScopeRead) {
		t.Fatal("identity should carry read scope")
	}
	if ident.Has(ScopeSubmit) {
		t.Fatal("a read-only token must not carry submit scope")
	}

	// Only the hash is stored: the plaintext must not be recoverable from the
	// file, which is what makes a leaked backup harmless.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := raw[strings.Index(raw, ".")+1:]
	if strings.Contains(string(data), secret) {
		t.Fatalf("the plaintext secret is in %s", path)
	}
	if !strings.Contains(string(data), rec.SecretHash) {
		t.Fatalf("the hash is missing from %s: %s", path, data)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("registry mode = %o, want 600", perm)
	}
}

// A session is the person, so it is not narrowed; a token carries exactly what
// was issued.
func TestIdentityScopeSemantics(t *testing.T) {
	human := HumanIdentity("alice")
	if human.Agent {
		t.Fatal("a session identity is not an agent")
	}
	if !human.Has(ScopeSubmit) || !human.Has(ScopeRead) {
		t.Fatal("a session carries every scope")
	}

	agent := Identity{User: "alice", Agent: true, Scopes: ScopeSet{ScopeRead}}
	if !agent.Has(ScopeRead) || agent.Has(ScopeSubmit) {
		t.Fatal("an agent identity carries only its scopes")
	}
	if got := agent.Describe(); !strings.Contains(got, "alice") || !strings.Contains(got, "read") {
		t.Fatalf("Describe() = %q", got)
	}
}

func TestAuthenticateRejectsBadCredentials(t *testing.T) {
	s, _ := newTestStore(t)
	_, raw := mustCreate(t, s, CreateParams{User: "alice"})

	tampered := raw[:len(raw)-1] + flip(raw[len(raw)-1])

	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"no header", "", "no bearer credential"},
		{"basic scheme", "Basic YWxpY2U6cHc=", "unsupported Authorization scheme"},
		{"empty bearer", "Bearer ", "empty bearer token"},
		{"not a token", "Bearer hunter2", "malformed"},
		{"wrong prefix", "Bearer sk-abcdefgh.zzz", "malformed"},
		{"bad id", "Bearer srcos_ZZZZZZZZ.zzz", "malformed"},
		{"no separator", "Bearer srcos_abcdefgh", "malformed"},
		{"empty secret", "Bearer srcos_abcdefgh.", "malformed"},
		{"unknown id", "Bearer srcos_abcdefgh.AAAA", "unknown agent token"},
		{"tampered secret", "Bearer " + tampered, "invalid agent token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/tools", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			_, err := s.Authenticate(req)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
			if tc.header == "" && err != ErrNoCredential {
				t.Fatalf("a missing header must be ErrNoCredential, got %v", err)
			}
		})
	}
}

// flip changes one byte, to test that a tampered token is rejected.
func flip(b byte) string {
	if b == 'a' {
		return "b"
	}
	return "a"
}

func TestExpiredTokenIsRejected(t *testing.T) {
	s, _ := newTestStore(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	_, raw := mustCreate(t, s, CreateParams{
		User:      "bob",
		ExpiresAt: now.Add(24 * time.Hour),
		Now:       now,
	})

	if _, err := s.verifyAt(raw, now.Add(time.Hour)); err != nil {
		t.Fatalf("a token inside its window must work: %v", err)
	}
	_, err := s.verifyAt(raw, now.Add(25*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v, want an expiry rejection", err)
	}
	// The boundary belongs to the expiry: at exactly ExpiresAt it is over.
	if _, err := s.verifyAt(raw, now.Add(24*time.Hour)); err == nil {
		t.Fatal("a token must be expired at its expiry instant")
	}
}

// Create refuses what it cannot honour: an unknown scope, and an allowlist whose
// scope is missing (which would silently do nothing).
func TestCreateRejectsWhatItCannotIssue(t *testing.T) {
	s, path := newTestStore(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		p    CreateParams
		want string
	}{
		{"unknown scope", CreateParams{User: "alice", Scopes: []Scope{"admin"}}, "unknown scope"},
		{"allowlist without submit", CreateParams{User: "alice", SubmitTools: []string{"ticker"}}, "needs the \"submit\" scope"},
		{"allowlist with a bad id", CreateParams{
			User: "alice", Scopes: []Scope{ScopeSubmit}, SubmitTools: []string{"Not A Tool"}}, "must match"},
		{"invalid user", CreateParams{User: "not a user"}, "invalid username"},
		{"empty user", CreateParams{}, "invalid username"},
		{"past expiry", CreateParams{User: "alice", ExpiresAt: now.Add(-time.Hour), Now: now}, "not in the future"},
		{"long label", CreateParams{User: "alice", Label: strings.Repeat("x", maxLabelLen+1)}, "label too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Create(tc.p); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	// Nothing was written: a refused create must not leave a file behind.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused create wrote %s: %v", path, err)
	}
}

// The submit scope is the phase-2 capability, and it implies read: a credential
// that can start work but not watch it would only let its holder act blind.
func TestSubmitScopeImpliesReadAndHonoursTheAllowlist(t *testing.T) {
	s, _ := newTestStore(t)
	rec, raw := mustCreate(t, s, CreateParams{
		User: "alice", Scopes: []Scope{ScopeSubmit}, SubmitTools: []string{"ticker", "ticker"},
	})
	if got := ScopeSet(rec.Scopes).String(); got != "read,submit" {
		t.Fatalf("scopes = %q, want read,submit", got)
	}
	if len(rec.SubmitTools) != 1 || rec.SubmitTools[0] != "ticker" {
		t.Fatalf("submit_tools = %v, want a deduplicated [ticker]", rec.SubmitTools)
	}

	ident, err := s.verifyAt(raw, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ident.Has(ScopeRead) || !ident.Has(ScopeSubmit) {
		t.Fatalf("scopes = %v", ident.Scopes)
	}
	if !ident.CanSubmitTool("ticker") {
		t.Error("the allowlisted tool must be submittable")
	}
	if ident.CanSubmitTool("other") {
		t.Error("a tool outside the allowlist must not be submittable")
	}

	// An empty allowlist means every tool the owner may use.
	_, rawOpen := mustCreate(t, s, CreateParams{User: "alice", Scopes: []Scope{ScopeSubmit}})
	open, err := s.verifyAt(rawOpen, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !open.CanSubmitTool("anything") {
		t.Error("an empty allowlist must allow any tool")
	}

	// A read-only token cannot submit, whatever the tool.
	_, rawRead := mustCreate(t, s, CreateParams{User: "alice"})
	readOnly, err := s.verifyAt(rawRead, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.CanSubmitTool("ticker") {
		t.Error("a read-only token must not submit")
	}

	// A human session is not narrowed (the Grant policy is the bound).
	if !HumanIdentity("alice").CanSubmitTool("ticker") {
		t.Error("a session must be able to submit")
	}
}

func TestRevoke(t *testing.T) {
	s, _ := newTestStore(t)
	rec, raw := mustCreate(t, s, CreateParams{User: "alice"})
	if _, err := s.verifyAt(raw, time.Now()); err != nil {
		t.Fatal(err)
	}

	removed, found, err := s.Revoke(rec.ID)
	if err != nil || !found {
		t.Fatalf("Revoke = %+v, %v, %v", removed, found, err)
	}
	if _, err := s.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("a revoked token must stop working")
	}
	if _, found, _ := s.Revoke(rec.ID); found {
		t.Fatal("revoking twice must report not-found")
	}
	if _, _, err := s.Revoke("nosuchid"); err != nil {
		t.Fatalf("revoking an unknown id is not an error: %v", err)
	}
}

func TestRevokeAllFor(t *testing.T) {
	s, _ := newTestStore(t)
	mustCreate(t, s, CreateParams{User: "alice", Label: "one"})
	mustCreate(t, s, CreateParams{User: "alice", Label: "two"})
	mustCreate(t, s, CreateParams{User: "bob"})

	removed, err := s.RevokeAllFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Fatalf("revoked %d tokens, want 2", len(removed))
	}
	left := s.Tokens()
	if len(left) != 1 || left[0].User != "bob" {
		t.Fatalf("remaining tokens = %+v", left)
	}
	// A user with nothing left is a no-op, not an error.
	if removed, err := s.RevokeAllFor("alice"); err != nil || len(removed) != 0 {
		t.Fatalf("RevokeAllFor = %v, %v", removed, err)
	}
}

// Create/revoke in one process must be visible to a gateway that has been
// running all along — that is what makes `srcos token revoke` immediate.
func TestChangesAreVisibleWithoutRestart(t *testing.T) {
	s, path := newTestStore(t)
	gateway := New(path) // opened before the token exists

	rec, raw := mustCreate(t, s, CreateParams{User: "alice"})
	if _, err := gateway.verifyAt(raw, time.Now()); err != nil {
		t.Fatalf("a running gateway did not pick up the new token: %v", err)
	}
	if _, _, err := s.Revoke(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("a running gateway kept honouring a revoked token")
	}
}

// Fail closed: a broken or deleted registry denies every agent until it is
// fixed, rather than serving whatever was loaded last.
func TestBrokenRegistryDeniesEverything(t *testing.T) {
	s, path := newTestStore(t)
	_, raw := mustCreate(t, s, CreateParams{User: "alice"})
	if _, err := s.verifyAt(raw, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("tokens: [ {id: this is not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil {
		t.Fatal("a malformed file must be an error")
	}
	if _, err := s.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("a malformed file must deny, not keep the old snapshot")
	}

	req := httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	if _, err := s.Authenticate(req); err == nil {
		t.Fatal("Authenticate must refuse while the registry is unreadable")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil {
		t.Fatalf("a missing registry is not an error: %v", err)
	}
	if _, err := s.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("a deleted registry must deny")
	}
}

// Broken registries are logged once, not once per request: the file is
// re-read on every authenticated request, so a report per request would let a
// broken file (or an attacker presenting garbage while it is broken) flood the
// log. The report comes back after the file is fixed and breaks again.
func TestBrokenRegistryIsLoggedOnce(t *testing.T) {
	s, path := newTestStore(t)
	_, raw := mustCreate(t, s, CreateParams{User: "alice"})

	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	if err := os.WriteFile(path, []byte("tokens: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := s.verifyAt(raw, time.Now()); err == nil {
			t.Fatal("a broken registry must reject")
		}
	}
	if got := strings.Count(buf.String(), "rejecting every agent token"); got != 1 {
		t.Fatalf("the breakage was reported %d times, want 1: %s", got, buf.String())
	}

	// Fixed, then broken again: the report comes back.
	if err := os.WriteFile(path, []byte("tokens: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("the broken file took the token with it, so this should be unknown")
	}
	if err := os.WriteFile(path, []byte("tokens: [broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.verifyAt(raw, time.Now()); err == nil {
		t.Fatal("a broken registry must reject")
	}
	if got := strings.Count(buf.String(), "rejecting every agent token"); got != 2 {
		t.Fatalf("the breakage was reported %d times, want 2: %s", got, buf.String())
	}
}

// Every rejection path must be exercised at least once at the file level: the
// registry is a credential store, so a typo has to fail loudly.
func TestReloadRejectsInconsistentRegistries(t *testing.T) {
	hash := strings.Repeat("a", hashHexLen)
	created := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	base := func() []Token {
		return []Token{{
			ID: "abcdefgh", User: "alice", Scopes: []Scope{ScopeRead},
			CreatedAt: created, SecretHash: hash,
		}}
	}

	cases := map[string]func([]Token) []Token{
		"duplicate id": func(ts []Token) []Token {
			return append(ts, ts[0])
		},
		"unknown scope": func(ts []Token) []Token {
			ts[0].Scopes = []Scope{"admin"}
			return ts
		},
		"no scope": func(ts []Token) []Token {
			ts[0].Scopes = nil
			return ts
		},
		"duplicate scope": func(ts []Token) []Token {
			ts[0].Scopes = []Scope{ScopeRead, ScopeRead}
			return ts
		},
		"bad id": func(ts []Token) []Token {
			ts[0].ID = "ABC"
			return ts
		},
		"bad hash": func(ts []Token) []Token {
			ts[0].SecretHash = "not-a-hash"
			return ts
		},
		"invalid user": func(ts []Token) []Token {
			ts[0].User = "not a user"
			return ts
		},
		"missing created_at": func(ts []Token) []Token {
			ts[0].CreatedAt = time.Time{}
			return ts
		},
		"expiry before creation": func(ts []Token) []Token {
			ts[0].ExpiresAt = created.Add(-time.Hour)
			return ts
		},
		"label too long": func(ts []Token) []Token {
			ts[0].Label = strings.Repeat("x", maxLabelLen+1)
			return ts
		},
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateTokens(mutate(base())); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
	if err := validateTokens(base()); err != nil {
		t.Fatalf("the well-formed registry must validate: %v", err)
	}
}

func TestCreateMintsUniqueIDs(t *testing.T) {
	s, _ := newTestStore(t)
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		rec, raw := mustCreate(t, s, CreateParams{User: "alice"})
		if len(rec.ID) != 8 {
			t.Fatalf("id %q is not 8 characters", rec.ID)
		}
		if seen[rec.ID] {
			t.Fatalf("duplicate id %s", rec.ID)
		}
		seen[rec.ID] = true
		if id, ok := tokenID(raw); !ok || id != rec.ID {
			t.Fatalf("tokenID(%q) = %q, %v", raw, id, ok)
		}
	}
	if len(s.Tokens()) != 40 {
		t.Fatalf("snapshot has %d tokens", len(s.Tokens()))
	}
}

func TestScopesAreCanonicalAndDeduplicated(t *testing.T) {
	s, _ := newTestStore(t)
	rec, _, err := s.Create(CreateParams{User: "alice", Scopes: []Scope{ScopeRead, ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Scopes) != 1 || rec.Scopes[0] != ScopeRead {
		t.Fatalf("scopes = %v, want [read]", rec.Scopes)
	}
}

func TestHasBearerCredentials(t *testing.T) {
	cases := map[string]bool{
		"Bearer srcos_x.y": true,
		"bearer srcos_x.y": true,
		"BEARER  token":    true,
		// An empty credential must not count: it would skip the CSRF guard and
		// then fall back to the session cookie.
		"Bearer":    false,
		"Bearer ":   false,
		"Basic abc": false,
		"":          false,
	}
	for header, want := range cases {
		req := httptest.NewRequest("GET", "/api/tools", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		if got := HasBearerCredentials(req); got != want {
			t.Fatalf("HasBearerCredentials(%q) = %v, want %v", header, got, want)
		}
	}
}

// An instance credential is a subset by construction and lives in its own file,
// so the gateway never races a human's `token create` (A1).
func TestInstanceCredentials(t *testing.T) {
	dir := t.TempDir()
	cfg := New(filepath.Join(dir, "agent-tokens.yaml"))
	rt := New(filepath.Join(dir, "instance-tokens.yaml"))

	plaintext, err := rt.MintInstance("alice", "alice-web-svc", []string{"submit"}, []string{"demo"})
	if err != nil {
		t.Fatal(err)
	}
	req := func(tok string) *http.Request {
		r := httptest.NewRequest("GET", "/api/tools", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		return r
	}

	ident, err := rt.Authenticate(req(plaintext))
	if err != nil {
		t.Fatalf("runtime store: %v", err)
	}
	if ident.User != "alice" || !ident.Agent || ident.Instance != "alice-web-svc" {
		t.Fatalf("identity = %+v", ident)
	}
	// submit implies read, and the allowlist narrows by tool.
	if !ident.Has(ScopeRead) || !ident.Has(ScopeSubmit) {
		t.Fatalf("scopes = %v", ident.Scopes)
	}
	if !ident.CanSubmitTool("demo") || ident.CanSubmitTool("other") {
		t.Fatalf("allowlist = %v", ident.SubmitTools)
	}

	// The config store must not know it: two writers, two files.
	if _, err := cfg.Authenticate(req(plaintext)); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("the user store must not hold an instance credential: %v", err)
	}
	// The chain finds it.
	if _, err := (Chain{cfg, rt}).Authenticate(req(plaintext)); err != nil {
		t.Fatalf("chain: %v", err)
	}

	actor := ident.AuditActor()
	if actor.Kind != audit.KindAgentToken || actor.Instance != "alice-web-svc" {
		t.Fatalf("audit actor = %+v", actor)
	}

	// Revoking the instance kills the credential; revoking again is a no-op.
	if ok, err := rt.RevokeInstance("alice-web-svc"); err != nil || !ok {
		t.Fatalf("revoke = %v %v", ok, err)
	}
	if _, err := (Chain{cfg, rt}).Authenticate(req(plaintext)); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("after revoke the chain must reject: %v", err)
	}
	if ok, _ := rt.RevokeInstance("alice-web-svc"); ok {
		t.Fatal("a second revoke should report false")
	}
	if n := len(rt.InstanceTokens()); n != 0 {
		t.Fatalf("instance tokens left = %d", n)
	}
	// Minting without an instance id is refused: the binding is the point.
	if _, err := rt.MintInstance("alice", "  ", nil, nil); err == nil {
		t.Fatal("MintInstance must require an instance id")
	}
}
