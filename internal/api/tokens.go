package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/agenttoken"
)

// This file is the credential-management surface: the self-service page's
// backend.
//
//	GET    /api/tokens         → the caller's own tokens
//	POST   /api/tokens         → mint one (the plaintext is returned once)
//	DELETE /api/tokens/<id>    → revoke one of the caller's own
//
// Two rules shape it:
//
//   - **Session only.** A program must not be able to manage credentials: a
//     leaked submit token could otherwise mint itself a fresh, longer-lived one
//     and outlive its own revocation. Managing credentials is a human act, so an
//     agent identity is refused here even when it carries the submit scope.
//   - **Your own, never another's.** Every answer is scoped to the session user.
//     A token id that belongs to someone else is "not found" — the same rule the
//     read side applies to instances, for the same reason.
//
// What a token can *do* is bounded elsewhere and unchanged: the Grant policy is
// the outer bound (ADR-019's subset rule), and `submit_tools` narrows it
// further. The create call therefore refuses an allowlist naming a tool the user
// cannot use today — an allowlist that could never fire is a trap, not a feature.

// createTokenBody is the request shape for POST /api/tokens.
type createTokenBody struct {
	Label string `json:"label"`
	// Scopes may name "read" and/or "submit"; empty means read.
	Scopes []string `json:"scopes"`
	// Tools narrows the submit scope (empty = every tool the user may use).
	Tools []string `json:"tools"`
	// Expires is "90d" | "12h" | "2026-12-21" | "never".
	Expires string `json:"expires"`
}

// createTokenResponse carries the plaintext exactly once.
type createTokenResponse struct {
	Token agenttoken.View `json:"token"`
	// Plaintext is shown here and never stored: only its SHA-256 is kept.
	Plaintext string `json:"plaintext"`
}

// handleListTokens returns the caller's own tokens.
func (h *Handler) handleListTokens(w http.ResponseWriter, ident agenttoken.Identity) {
	if !h.requireSessionOnly(w, ident) {
		return
	}
	writeJSON(w, 200, map[string]any{"tokens": h.tokenViews(ident.User)})
}

// handleCreateToken mints one for the session user.
func (h *Handler) handleCreateToken(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity) {
	if !h.requireSessionOnly(w, ident) {
		return
	}
	if h.opts.AgentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent tokens are not configured on this gateway"})
		return
	}
	var body createTokenBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}

	scopes := make([]agenttoken.Scope, 0, len(body.Scopes))
	for _, s := range body.Scopes {
		scopes = append(scopes, agenttoken.Scope(strings.TrimSpace(s)))
	}
	store := h.opts.AgentTokens
	// Normalize through the store's own rules first (this is what expands
	// submit → read); an unknown scope or a stray allowlist is refused there.
	expiresAt, err := agenttoken.ParseExpiry(body.Expires, time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// An allowlist must be inside what the user may use *now*; the submit-time
	// check still runs later (grants change), this only stops an entry that
	// could never fire.
	tools, err := h.validatedTools(ident.User, body.Tools)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// A runaway page must not be able to mint hundreds of credentials for one
	// account; revocation is manual, so the ceiling is low and explicit.
	const maxTokensPerUser = 20
	if n := len(h.tokenViews(ident.User)); n >= maxTokensPerUser {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("you already have %d tokens; revoke one before creating another", n),
		})
		return
	}

	rec, plaintext, err := store.Create(agenttoken.CreateParams{
		User:        ident.User,
		Label:       body.Label,
		Scopes:      scopes,
		SubmitTools: tools,
		ExpiresAt:   expiresAt,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	scopeNames := make([]string, 0, len(rec.Scopes))
	for _, s := range rec.Scopes {
		scopeNames = append(scopeNames, string(s))
	}
	expires := "never"
	if !rec.ExpiresAt.IsZero() {
		expires = rec.ExpiresAt.Format(time.RFC3339)
	}
	h.auditChange(r, ident, "token.create", "token", rec.ID, map[string]any{
		"label":   rec.Label,
		"scopes":  strings.Join(scopeNames, ","),
		"tools":   strings.Join(rec.SubmitTools, ","),
		"expires": expires,
	})
	writeJSON(w, http.StatusCreated, createTokenResponse{
		Token:     rec.View(time.Now(), time.Time{}),
		Plaintext: plaintext,
	})
}

// handleRevokeToken removes one of the caller's own tokens.
func (h *Handler) handleRevokeToken(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity, id string) {
	if !h.requireSessionOnly(w, ident) {
		return
	}
	if h.opts.AgentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "agent tokens are not configured on this gateway"})
		return
	}
	id = strings.TrimSpace(id)
	// Ownership first, and "not yours" is "not found": a caller must not be able
	// to probe which token ids exist.
	if !h.ownsToken(ident.User, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such token"})
		return
	}
	if _, found, err := h.opts.AgentTokens.Revoke(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	} else if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such token"})
		return
	}
	h.auditChange(r, ident, "token.revoke", "token", id, nil)
	writeJSON(w, 200, map[string]string{"revoked": id})
}

// requireSessionOnly refuses an agent identity.
func (h *Handler) requireSessionOnly(w http.ResponseWriter, ident agenttoken.Identity) bool {
	if ident.Agent {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "managing credentials requires a browser session; an agent token cannot mint or revoke tokens",
		})
		return false
	}
	return true
}

// tokenViews lists one user's tokens, newest first.
func (h *Handler) tokenViews(user string) []agenttoken.View {
	if h.opts.AgentTokens == nil {
		return []agenttoken.View{}
	}
	return h.opts.AgentTokens.ViewsFor(user, time.Now())
}

// ownsToken reports whether id belongs to user.
func (h *Handler) ownsToken(user, id string) bool {
	if h.opts.AgentTokens == nil || id == "" {
		return false
	}
	for _, t := range h.opts.AgentTokens.Tokens() {
		if t.ID == id {
			return t.User == user
		}
	}
	return false
}

// validatedTools checks a submit allowlist against the tools this user may
// actually use, and reports what is wrong in the caller's own terms.
func (h *Handler) validatedTools(user string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	manifests, err := h.reader().VisibleManifests(user)
	if err != nil {
		return nil, err
	}
	usable := make(map[string]bool, len(manifests))
	for _, t := range manifests {
		usable[t.ID] = true
	}
	out := make([]string, 0, len(ids))
	var unknown []string
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if !usable[id] {
			unknown = append(unknown, id)
			continue
		}
		out = append(out, id)
	}
	if len(unknown) > 0 {
		// Name them: the page can then say which entry was wrong, and the rule
		// is stated in the same breath (a token narrows, it never widens).
		return nil, fmt.Errorf("these tools are not authorized for you: %s "+
			"(a token's allowlist can only narrow what you may already use)", strings.Join(unknown, ", "))
	}
	return out, nil
}
