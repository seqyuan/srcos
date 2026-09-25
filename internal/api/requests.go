package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/accessrequest"
	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/config"
)

// This file is the tool-access request surface (B3): a user asking for a tool
// they cannot use, and an administrator's decision about it.
//
// A request grants nothing. Approval writes a grant through the same
// grant.Policy.AddUserToGrant the CLI uses, so "why can alice use this tool"
// still points at a line in grants.yaml — the feature adds a way to *ask*, not
// a second way to *be* authorized.
//
// Both halves are session-only. A program must not grow a credential by asking
// for one: the request surface is a human workflow, and an agent acts as its
// user's existing permissions (ADR-019's subset rule).

// dataDir is where runtime state lives (data/), a sibling of config/.
func (h *Handler) dataDir() string { return config.DataDir(h.opts.ConfigDir) }

// handleListRequests returns the caller's own requests, oldest first.
func (h *Handler) handleListRequests(w http.ResponseWriter, ident agenttoken.Identity) {
	if !h.requireSessionOnly(w, ident) {
		return
	}
	reqs, err := accessrequest.ListFor(h.dataDir(), ident.User)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if reqs == nil {
		reqs = []accessrequest.Request{} // JSON [], not null
	}
	writeJSON(w, 200, map[string]any{"requests": reqs})
}

// handleCreateRequest files a request for a requestable tool the caller lacks.
//
// A second request for the same tool returns the existing pending one (200
// instead of 201): an impatient click must not queue up work for the
// administrator to prune.
func (h *Handler) handleCreateRequest(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity) {
	if !h.requireSessionOnly(w, ident) {
		return
	}
	if h.opts.Policy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this gateway has no authorization policy, so nothing is requestable",
		})
		return
	}
	var body struct {
		Tool   string `json:"tool"`
		Reason string `json:"reason"`
	}
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	toolID := strings.TrimSpace(body.Tool)
	if toolID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tool is required"})
		return
	}
	// The tool must exist, not already be usable, and be marked requestable —
	// three distinct refusals, each with its own message. "Already usable" comes
	// first: it is the more useful answer, and it avoids mentioning
	// requestability for a tool the caller has anyway.
	t, err := h.manifestForAdmin(toolID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such tool: " + toolID})
		return
	}
	if h.opts.Policy.Allowed(ident.User, t.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "you already have access to " + t.ID,
		})
		return
	}
	if !h.opts.Policy.Requestable(t.ID) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "tool " + t.ID + " is not open to requests",
		})
		return
	}

	req, created, err := accessrequest.Create(h.dataDir(), ident.User, t.ID, body.Reason, time.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		h.auditChange(r, ident, "request.create", "request", req.ID, map[string]any{"tool": t.ID})
	}
	writeJSON(w, status, map[string]any{"request": req})
}

// adminListRequests returns every request (optionally filtered by state).
func (h *Handler) adminListRequests(w http.ResponseWriter, r *http.Request) {
	reqs, err := accessrequest.List(h.dataDir())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	pending := 0
	for _, q := range reqs {
		if q.State == accessrequest.Pending {
			pending++
		}
	}
	if state := strings.TrimSpace(r.URL.Query().Get("state")); state != "" {
		var filtered []accessrequest.Request
		for _, q := range reqs {
			if string(q.State) == state {
				filtered = append(filtered, q)
			}
		}
		reqs = filtered
	}
	if reqs == nil {
		reqs = []accessrequest.Request{}
	}
	writeJSON(w, 200, map[string]any{"requests": reqs, "pending": pending})
}

// adminApproveRequest grants access and records the decision.
//
// The grant is applied and persisted *before* the request is marked approved:
// if the policy cannot be saved, the request stays pending (recoverable) rather
// than reading "approved" with no grant behind it.
func (h *Handler) adminApproveRequest(w http.ResponseWriter, r *http.Request, username, id string) {
	h.decideRequest(w, r, username, id, true)
}

// adminDenyRequest refuses a request, optionally with a note.
func (h *Handler) adminDenyRequest(w http.ResponseWriter, r *http.Request, username, id string) {
	h.decideRequest(w, r, username, id, false)
}

func (h *Handler) decideRequest(w http.ResponseWriter, r *http.Request, username, id string, approve bool) {
	if h.opts.Policy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this gateway has no authorization policy",
		})
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	// A note is meaningful for a denial; an absent body is fine for approval.
	if r.ContentLength != 0 {
		if err := parseBody(r, &body); err != nil {
			writeBodyError(w, err)
			return
		}
	}

	req, err := accessrequest.Get(h.dataDir(), id)
	if err != nil {
		if errors.Is(err, accessrequest.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such request"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if req.Terminal() {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "request " + req.ID + " is already " + string(req.State),
		})
		return
	}

	if approve {
		// The tool package must still exist: approving a deleted tool would
		// write a grant nobody can act on.
		if _, err := h.manifestForAdmin(req.Tool); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "tool " + req.Tool + " no longer exists on this host",
			})
			return
		}
		h.opts.Policy.AddUserToGrant(req.Tool, req.User)
		if err := h.savePolicy(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	decided, err := accessrequest.Decide(h.dataDir(), req.ID, username, approve, body.Note, time.Now())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	action := "request.deny"
	if approve {
		action = "request.approve"
	}
	h.auditChange(r, agenttoken.HumanIdentity(username), action, "request", decided.ID, map[string]any{
		"tool": req.Tool,
		"user": req.User,
		"note": body.Note,
	})
	writeJSON(w, 200, map[string]any{"request": decided})
}
