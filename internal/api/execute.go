package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/execute"
)

// This file is the write surface's HTTP spelling for the two operations that
// are not a submission: cancelling one instance, and running a flow.
//
//	POST /api/jobs/<id>/cancel   {"..."}                  → stop one instance
//	POST /api/flows/<id>/run     {"samples":"a,b\n1,2"}   → start a flow run
//
// Both go through package execute, so the submit scope (and its per-tool
// allowlist) is enforced in one place, shared with the MCP server.

// handleCancelInstance stops one of the caller's instances.
func (h *Handler) handleCancelInstance(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity, id string) {
	res, err := h.opts.Execute.Cancel(r.Context(), ident, id)
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

// runFlowBody is the request shape for POST /api/flows/<id>/run.
//
// Samples travel as the CSV text rather than a file path: an agent has no
// filesystem, and the table is a sample list (small), not data.
type runFlowBody struct {
	Samples     string            `json:"samples"`
	Params      map[string]string `json:"params"`
	Concurrency int               `json:"concurrency"`
}

// handleRunFlow expands a flow over a sample table and starts it.
func (h *Handler) handleRunFlow(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity, flowID string) {
	var body runFlowBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	if body.Concurrency < 0 {
		writeJSON(w, 400, map[string]string{"error": "concurrency must not be negative"})
		return
	}
	res, err := h.opts.Execute.RunFlow(ident, execute.FlowRunRequest{
		Flow:        flowID,
		Samples:     body.Samples,
		Params:      body.Params,
		Concurrency: body.Concurrency,
	})
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// jobCancelID extracts the instance id from /api/jobs/<id>/cancel.
//
// Same shape discipline as the log endpoint: the id must be exactly one
// segment, so a traversal or an unexpected path shape is a 404 rather than
// something the store is asked to interpret.
func jobCancelID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/jobs/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/cancel")
	return cleanID(id, ok)
}

// flowRunID extracts the flow id from /api/flows/<id>/run.
func flowRunID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/flows/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/run")
	return cleanID(id, ok)
}

// cleanID accepts a single, non-empty, separator-free id.
func cleanID(id string, ok bool) (string, bool) {
	if !ok || id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", false
	}
	return id, true
}
