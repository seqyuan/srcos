// Package agenttoken is the agent credential surface: program-shaped
// credentials for the agent / MCP side of the platform (ADR-019).
//
// An agent is a program, so it cannot hold a browser session cookie. It
// carries a bearer token instead. Two properties shape everything here:
//
//   - **Only hashes are stored.** config/agent-tokens.yaml holds the SHA-256 of
//     each token, never the token. The plaintext is printed once, by
//     `srcos token create`, and cannot be recovered afterwards — so a leaked
//     copy of the file (or a backup of it) does not let anyone call the API.
//
//   - **A token never widens access.** It authenticates *as its user*, and the
//     Grant policy still decides what that user may see and run. Scopes can
//     only narrow that further, which is exactly the subset rule of ADR-019.
//
// The registry is read from disk on change rather than held in memory
// forever, so `srcos token create` / `revoke` take effect on the running
// gateway without a restart. Every failure path denies: a malformed file
// leaves the store empty instead of serving the previous snapshot.
package agenttoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/config"
)

const (
	// TokenPrefix marks a plaintext value as an agent token, so a password or
	// a session value pasted by mistake is rejected with a clear message
	// instead of a mysterious 401. It also makes a token greppable in a config
	// file or a leak report.
	TokenPrefix = "srcos_"

	// idBytes / secretBytes are the sizes of the public id and the secret. The
	// secret is 256 bits of entropy, which is why a plain SHA-256 is enough:
	// there is nothing to brute-force, so no salt or KDF is needed (unlike a
	// password, which is low-entropy by construction).
	idBytes     = 5  // 8 base32 chars
	secretBytes = 32 // 43 base64url chars
	hashHexLen  = 64 // SHA-256 hex
	maxLabelLen = 64

	// separator sits between id and secret. '.' appears in neither base32 nor
	// base64url, so splitting on it is unambiguous.
	separator = "."
)

// Scope is a capability an agent token carries.
type Scope string

const (
	// ScopeRead allows the read-only surface: catalogues, paths, instance
	// state, logs, artifacts. It is the default, and the only scope that can be
	// issued today.
	ScopeRead Scope = "read"

	// ScopeSubmit is reserved for the second phase of ADR-019 (submit / cancel
	// / run_flow) and is still undecided in detail (see roadmap §8 #9), so
	// `Create` refuses to issue it rather than hand out a credential whose
	// scope silently does nothing.
	ScopeSubmit Scope = "submit"
)

// KnownScopes lists every scope the platform understands, in display order.
var KnownScopes = []Scope{ScopeRead, ScopeSubmit}

func isKnownScope(s Scope) bool {
	for _, k := range KnownScopes {
		if s == k {
			return true
		}
	}
	return false
}

func scopeNames(scopes []Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, s := range scopes {
		parts = append(parts, string(s))
	}
	return strings.Join(parts, ",")
}

// ScopeSet is the set of scopes an identity carries.
type ScopeSet []Scope

// Has reports whether the set contains a scope.
func (s ScopeSet) Has(want Scope) bool {
	for _, have := range s {
		if have == want {
			return true
		}
	}
	return false
}

// String renders the set as "read,submit" for logs and tables.
func (s ScopeSet) String() string { return scopeNames([]Scope(s)) }

// Identity is who a request acts as: a human session, or an agent token acting
// for its user.
type Identity struct {
	User string
	// Agent is true when the request came from an agent token. Human sessions
	// carry no TokenID and no scopes — the person is not narrowed.
	Agent   bool
	TokenID string
	Label   string
	Scopes  ScopeSet
}

// HumanIdentity is the identity of a browser session: the whole user.
func HumanIdentity(user string) Identity { return Identity{User: user} }

// Has reports whether the identity carries a scope. Sessions carry all of
// them (they are the person); tokens carry exactly what was issued.
func (i Identity) Has(s Scope) bool {
	if !i.Agent {
		return true
	}
	return i.Scopes.Has(s)
}

// Describe renders the identity for an audit log line.
func (i Identity) Describe() string {
	if !i.Agent {
		return i.User
	}
	who := fmt.Sprintf("%s via token %s", i.User, i.TokenID)
	if i.Label != "" {
		who += " (" + i.Label + ")"
	}
	return who + " [" + i.Scopes.String() + "]"
}

// Token is one issued agent token. The plaintext exists only in the caller's
// hands; this record keeps the id, the owner, the scopes and the hash.
type Token struct {
	// ID is the public, non-secret identifier used by `token list|revoke`.
	ID   string `yaml:"id"`
	User string `yaml:"user"`
	// Label is what the token is for ("annovibe", "nightly sync"). Operators
	// revoke by label far more often than by id, so it is part of the record.
	Label  string  `yaml:"label,omitempty"`
	Scopes []Scope `yaml:"scopes"`
	// CreatedAt / ExpiresAt are seconds-resolution UTC. A zero ExpiresAt means
	// the token does not expire.
	CreatedAt time.Time `yaml:"created_at"`
	ExpiresAt time.Time `yaml:"expires_at,omitempty"`
	// SecretHash is SHA-256 (hex) of the *whole* plaintext token.
	SecretHash string `yaml:"secret_hash"`
}

// Expired reports whether the token is past its expiry at `now`.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// Status is a one-word state for `token list`.
func (t Token) Status(now time.Time) string {
	if t.Expired(now) {
		return "expired"
	}
	return "active"
}

// file is the on-disk shape of config/agent-tokens.yaml.
type file struct {
	Tokens []Token `yaml:"tokens"`
}

// ErrNoCredential means the request carried no bearer token at all. Callers
// distinguish it from a rejection so a session cookie can still be tried;
// every other error means "a token was presented and refused".
var ErrNoCredential = errors.New("no bearer credential")

var (
	idPattern   = regexp.MustCompile(`^[a-z2-7]{8}$`)
	hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// idEncoding is lowercase base32 (RFC 4648 alphabet) without padding.
	idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
)

// ─────────────────────────────────────────────────────────────────────────
// Store
// ─────────────────────────────────────────────────────────────────────────

// Store is the token registry backed by one YAML file.
//
// Authentication always re-reads the file rather than trusting an in-memory
// snapshot. The file is a credential store and it is small: reading it once
// per authenticated request costs a fraction of a millisecond and removes a
// whole class of staleness — an mtime-based cache can miss a revoke on a
// filesystem with coarse timestamps, and "the token I revoked is still
// accepted" is the one bug this surface cannot afford.
//
// The snapshot that does exist (tokens/byID) is what a caller sees after an
// explicit Reload: the CLI lists from it, and Create/Revoke update it.
type Store struct {
	path  string
	usage *Usage

	mu     sync.RWMutex
	tokens []Token
	byID   map[string]Token

	// lastReloadErr deduplicates the "registry is broken" log line: the file
	// is re-read on every authenticated request, and a broken file must be
	// reported once rather than once per request.
	lastReloadErr string
}

// New returns an empty store for a registry path. Call Reload to populate the
// snapshot (Authenticate reloads on its own).
func New(path string) *Store {
	return &Store{path: path, byID: map[string]Token{}}
}

// Path is the registry file the store reads and writes.
func (s *Store) Path() string { return s.path }

// AttachUsage makes the gateway record "last used" times. The CLI does not
// attach one: only the process answering requests knows when a token is used.
func (s *Store) AttachUsage(u *Usage) { s.usage = u }

// Reload re-reads the registry, replacing the in-memory snapshot.
//
// A missing file is an empty registry: that is the normal state before the
// first `srcos token create`. A malformed file is an error AND leaves the
// store empty — a typo in a hand-edited file must deny every agent rather than
// keep serving the snapshot from before the edit.
func (s *Store) Reload() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		s.clear()
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("%s: %w", s.path, err)
	}

	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		s.clear()
		return fmt.Errorf("%s: %w", s.path, err)
	}
	if err := validateTokens(f.Tokens); err != nil {
		s.clear()
		return fmt.Errorf("%s: %w", s.path, err)
	}
	s.install(f.Tokens)
	return nil
}

// Tokens returns a copy of the snapshot, oldest first.
func (s *Store) Tokens() []Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	return out
}

// Authenticate resolves the request's bearer credential into an identity.
//
// ErrNoCredential means no credential was presented, so the caller may fall
// back to a session cookie. Any other error means a credential was presented
// and refused, and the caller must not fall back: a program that sent a bad
// token should get a clear failure, not the browser session behind it.
func (s *Store) Authenticate(r *http.Request) (Identity, error) {
	return s.authenticateRequest(r, time.Now())
}

// authenticateRequest resolves one request at a given instant.
func (s *Store) authenticateRequest(r *http.Request, now time.Time) (Identity, error) {
	scheme, credential := parseAuthorization(r.Header.Get("Authorization"))
	switch {
	case scheme == "":
		return Identity{}, ErrNoCredential
	case scheme != "bearer":
		return Identity{}, fmt.Errorf("unsupported Authorization scheme %q (agent tokens use Bearer)", scheme)
	case credential == "":
		return Identity{}, errors.New("empty bearer token")
	}
	return s.verifyAt(credential, now)
}

// verifyAt re-reads the registry and checks one plaintext token at `now`.
//
// `now` is a parameter so expiry can be tested without sleeping; the read is
// inside so that no caller can verify against a stale snapshot (see Store).
func (s *Store) verifyAt(raw string, now time.Time) (Identity, error) {
	// Shape first: it is cheap, and garbage does not deserve a file read.
	id, ok := tokenID(raw)
	if !ok {
		return Identity{}, fmt.Errorf("malformed agent token (want %s<id>%s<secret>)", TokenPrefix, separator)
	}

	if err := s.Reload(); err != nil {
		s.logReloadErr(err)
		return Identity{}, errors.New("agent token registry is unreadable; see the gateway log")
	}

	s.mu.RLock()
	rec, found := s.byID[id]
	s.mu.RUnlock()
	if !found {
		return Identity{}, errors.New("unknown agent token")
	}

	// Compare the hash before the expiry so a valid-but-expired token gets the
	// accurate message; neither order reveals anything to someone without the
	// secret.
	sum := sha256.Sum256([]byte(raw))
	stored, err := hex.DecodeString(rec.SecretHash)
	if err != nil || subtle.ConstantTimeCompare(sum[:], stored) != 1 {
		return Identity{}, errors.New("invalid agent token")
	}
	if rec.Expired(now) {
		return Identity{}, fmt.Errorf("agent token expired at %s",
			rec.ExpiresAt.UTC().Format(time.RFC3339))
	}

	if s.usage != nil {
		if err := s.usage.Touch(rec.ID, now); err != nil {
			// Usage is a hint, never a reason to reject a valid credential.
			log.Printf("[srcos] agent token usage: %v", err)
		}
	}
	return Identity{
		User:    rec.User,
		Agent:   true,
		TokenID: rec.ID,
		Label:   rec.Label,
		Scopes:  ScopeSet(rec.Scopes),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// issue / revoke
// ─────────────────────────────────────────────────────────────────────────

// CreateParams describes a token to mint. Zero fields mean "the default":
// scopes default to read, ExpiresAt zero means no expiry, Now zero means the
// wall clock (a parameter so tests can pin it).
type CreateParams struct {
	User      string
	Label     string
	Scopes    []Scope
	ExpiresAt time.Time
	Now       time.Time
}

// Create mints a token, records its hash and returns the plaintext token,
// which exists nowhere else after this call.
//
// User existence is the caller's check (the CLI verifies the account exists,
// the API has no such call), because this package deliberately does not know
// where accounts live.
func (s *Store) Create(p CreateParams) (Token, string, error) {
	if !config.IsValidUsername(p.User) {
		return Token{}, "", fmt.Errorf("invalid username %q", p.User)
	}
	label := strings.TrimSpace(p.Label)
	if len(label) > maxLabelLen {
		return Token{}, "", fmt.Errorf("label too long (max %d characters)", maxLabelLen)
	}

	scopes, err := normalizeScopes(p.Scopes)
	if err != nil {
		return Token{}, "", err
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Second)
	expires := time.Time{}
	if !p.ExpiresAt.IsZero() {
		expires = p.ExpiresAt.UTC().Truncate(time.Second)
		if !expires.After(now) {
			return Token{}, "", fmt.Errorf("expiry %s is not in the future",
				expires.Format(time.RFC3339))
		}
	}

	// Read-modify-write against the file as it is now, so a token created by
	// another terminal since this store was opened is not dropped.
	if err := s.Reload(); err != nil {
		return Token{}, "", err
	}

	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Token{}, "", fmt.Errorf("generate token secret: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.newIDLocked()
	if err != nil {
		return Token{}, "", err
	}
	raw := TokenPrefix + id + separator + base64.RawURLEncoding.EncodeToString(secret)
	sum := sha256.Sum256([]byte(raw))
	rec := Token{
		ID:         id,
		User:       p.User,
		Label:      label,
		Scopes:     scopes,
		CreatedAt:  now,
		ExpiresAt:  expires,
		SecretHash: hex.EncodeToString(sum[:]),
	}

	if err := s.commitLocked(append(append([]Token{}, s.tokens...), rec)); err != nil {
		return Token{}, "", err
	}
	return rec, raw, nil
}

// Revoke removes a token by id and reports whether it existed.
func (s *Store) Revoke(id string) (Token, bool, error) {
	if err := s.Reload(); err != nil {
		return Token{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]Token, 0, len(s.tokens))
	var removed Token
	found := false
	for _, t := range s.tokens {
		if t.ID == id && !found {
			removed, found = t, true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		return Token{}, false, nil
	}
	if err := s.commitLocked(kept); err != nil {
		return Token{}, false, err
	}
	return removed, true, nil
}

// RevokeAllFor removes every token owned by a user. It is what `srcos del`
// calls: a deleted account must not leave a working credential behind, and
// recreating the same name must not resurrect the old token.
func (s *Store) RevokeAllFor(user string) ([]Token, error) {
	if err := s.Reload(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := make([]Token, 0, len(s.tokens))
	var removed []Token
	for _, t := range s.tokens {
		if t.User == user {
			removed = append(removed, t)
			continue
		}
		kept = append(kept, t)
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := s.commitLocked(kept); err != nil {
		return nil, err
	}
	return removed, nil
}

// newIDLocked mints an id that is not in use. Callers hold the write lock.
func (s *Store) newIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		b := make([]byte, idBytes)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generate token id: %w", err)
		}
		id := idEncoding.EncodeToString(b)
		if _, taken := s.byID[id]; !taken {
			return id, nil
		}
	}
	return "", errors.New("could not find a free token id")
}

// ─────────────────────────────────────────────────────────────────────────
// snapshot bookkeeping
// ─────────────────────────────────────────────────────────────────────────

// commitLocked writes the registry and installs it in memory. Callers hold the
// write lock.
func (s *Store) commitLocked(tokens []Token) error {
	ordered := make([]Token, len(tokens))
	copy(ordered, tokens)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})

	if err := writeFile(s.path, ordered); err != nil {
		return err
	}
	s.installLocked(ordered)
	return nil
}

// install replaces the snapshot from outside the lock (Reload).
func (s *Store) install(tokens []Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installLocked(tokens)
	// A successful read clears the "broken file" state, so the next breakage is
	// reported again. Deliberately not in installLocked: clear() uses that too,
	// and it must not erase the record that keeps a broken file from being
	// logged once per request.
	s.lastReloadErr = ""
}

func (s *Store) installLocked(tokens []Token) {
	byID := make(map[string]Token, len(tokens))
	for _, t := range tokens {
		byID[t.ID] = t
	}
	s.tokens = tokens
	s.byID = byID
}

// clear empties the snapshot: the deny-everything state.
func (s *Store) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installLocked(nil)
}

func (s *Store) logReloadErr(err error) {
	s.mu.Lock()
	seen := s.lastReloadErr == err.Error()
	s.lastReloadErr = err.Error()
	s.mu.Unlock()
	if !seen {
		log.Printf("[srcos] agent tokens: %v — rejecting every agent token until it is fixed", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// validation
// ─────────────────────────────────────────────────────────────────────────

// validateTokens checks a registry's internal consistency. Anything
// questionable is an error: this file is a credential store, so a typo must
// fail loudly instead of half-working.
func validateTokens(tokens []Token) error {
	var problems []string
	bad := func(format string, a ...any) {
		problems = append(problems, fmt.Sprintf(format, a...))
	}

	seen := map[string]bool{}
	for i, t := range tokens {
		where := fmt.Sprintf("tokens[%d]", i)
		if t.ID != "" {
			where = fmt.Sprintf("tokens[%d] (id %s)", i, t.ID)
		}
		if !idPattern.MatchString(t.ID) {
			bad("%s: id must be %d lowercase base32 characters", where, 2*idBytes)
		}
		if seen[t.ID] {
			bad("%s: duplicate id", where)
		}
		seen[t.ID] = true

		if !config.IsValidUsername(t.User) {
			bad("%s: invalid user %q", where, t.User)
		}
		if len(t.Label) > maxLabelLen {
			bad("%s: label longer than %d characters", where, maxLabelLen)
		}
		if len(t.Scopes) == 0 {
			bad("%s: no scopes (a token that can do nothing is a mistake)", where)
		}
		scopeSeen := map[Scope]bool{}
		for _, s := range t.Scopes {
			if !isKnownScope(s) {
				bad("%s: unknown scope %q (known: %s)", where, s, scopeNames(KnownScopes))
			}
			if scopeSeen[s] {
				bad("%s: duplicate scope %q", where, s)
			}
			scopeSeen[s] = true
		}
		if !hashPattern.MatchString(strings.ToLower(t.SecretHash)) {
			bad("%s: secret_hash must be a SHA-256 hex digest", where)
		}
		if t.CreatedAt.IsZero() {
			bad("%s: created_at is required", where)
		}
		if !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(t.CreatedAt) {
			bad("%s: expires_at must be after created_at", where)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// normalizeScopes applies the default and rejects what cannot be issued.
func normalizeScopes(in []Scope) ([]Scope, error) {
	if len(in) == 0 {
		return []Scope{ScopeRead}, nil
	}
	seen := map[Scope]bool{}
	out := make([]Scope, 0, len(in))
	for _, s := range in {
		if !isKnownScope(s) {
			return nil, fmt.Errorf("unknown scope %q (known: %s)", s, scopeNames(KnownScopes))
		}
		if s == ScopeSubmit {
			return nil, fmt.Errorf("scope %q is reserved for the second phase of ADR-019 (submit / cancel / run_flow) and cannot be issued yet", s)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	// Canonical order, so the file and the audit lines do not depend on the
	// order flags happened to be typed in.
	sort.Slice(out, func(i, j int) bool { return scopeRank(out[i]) < scopeRank(out[j]) })
	return out, nil
}

func scopeRank(s Scope) int {
	for i, k := range KnownScopes {
		if s == k {
			return i
		}
	}
	return len(KnownScopes)
}

// ─────────────────────────────────────────────────────────────────────────
// token format / header parsing
// ─────────────────────────────────────────────────────────────────────────

// tokenID extracts the public id from a plaintext token. The id is a lookup
// key, so verification hashes one record instead of every record.
func tokenID(raw string) (string, bool) {
	rest, ok := strings.CutPrefix(raw, TokenPrefix)
	if !ok {
		return "", false
	}
	id, secret, ok := strings.Cut(rest, separator)
	if !ok || secret == "" || !idPattern.MatchString(id) {
		return "", false
	}
	return id, true
}

// parseAuthorization splits an Authorization header into a lowercase scheme
// and the credential. An empty scheme means "no header".
func parseAuthorization(header string) (scheme, credential string) {
	h := strings.TrimSpace(header)
	if h == "" {
		return "", ""
	}
	if i := strings.IndexByte(h, ' '); i >= 0 {
		return strings.ToLower(h[:i]), strings.TrimSpace(h[i+1:])
	}
	return strings.ToLower(h), ""
}

// HasBearerCredentials reports whether the request carries a Bearer
// credential with a value, valid or not.
//
// The API layer uses it to skip the browser CSRF guard: a bearer token is not
// an ambient credential (the browser never attaches one by itself), so a
// cross-site page cannot ride on it, and clients such as MCP harnesses may
// legitimately send their own Origin.
//
// The credential must be non-empty. A header of just "Bearer " would otherwise
// skip the origin check and then fall back to the session cookie (an empty
// credential is ErrNoCredential), which is exactly the CSRF hole the guard
// exists to close.
func HasBearerCredentials(r *http.Request) bool {
	scheme, credential := parseAuthorization(r.Header.Get("Authorization"))
	return scheme == "bearer" && credential != ""
}

// ─────────────────────────────────────────────────────────────────────────
// file IO
// ─────────────────────────────────────────────────────────────────────────

const headerComment = "# SRCOS agent token registry（程序凭据，ADR-019）\n" +
	"#\n" +
	"# 只存 SHA-256 哈希，不存明文：明文仅在 `srcos token create` 时打印一次，\n" +
	"# 之后无法恢复。因此这份文件（或它的备份）泄露并不等于凭据泄露。\n" +
	"#\n" +
	"# agent token 以「所属用户」的身份行事：Grant 授权策略依旧生效，scope 只能\n" +
	"# 在此基础上收窄（子集原则）。目前只能签发 `read`（只读面）。\n" +
	"#\n" +
	"# 由 `srcos token create|revoke` 维护；网关在每次校验时重新读取该文件，\n" +
	"# 所以 create / revoke 立即生效，无需重启。\n" +
	"# 请勿手工编辑（格式错误会让全部 token 失效，这是刻意的失败关闭）。\n\n"

// writeFile writes the registry atomically with 0600 permissions.
//
// Atomic because the gateway reads this file while requests are in flight: a
// half-written file would deny (fail closed) but also log a spurious error,
// and 0600 because a hash is still credential material.
func writeFile(path string, tokens []Token) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(file{Tokens: tokens})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(headerComment), data...), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
