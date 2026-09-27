package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the write surface's HTTP spelling for the operations that are
// not a submission: cancelling one instance, starting a service, and running a
// flow.
//
//	POST /api/jobs/<id>/cancel     {"..."}                  → stop one instance
//	POST /api/tools/<id>/start     {params}                → instantiate a service
//	POST /api/flows/<id>/run       {"samples":"a,b\n1,2"} → start a flow run
//
// All go through package execute, so the submit scope (and its per-tool
// allowlist) is enforced in one place, shared with the MCP server.

// startServiceBody is the request shape for POST /api/tools/<id>/start. The
// body is optional: a service with no inputs starts with `{}` or with nothing.
type startServiceBody struct {
	Name      string            `json:"name"`
	Params    map[string]any    `json:"params"`
	Tags      map[string]string `json:"tags"`
	Resources *tool.Resources   `json:"resources"`
}

// handleStartService instantiates a service tool for the caller.
//
// It is the self-service half of the service lifecycle: a user (or an agent
// token with submit scope) may start a service tool that their Grant allows,
// exactly as they may submit a task. The authorization, the quota and the
// one-live-instance rule all live in execute.StartService.
func (h *Handler) handleStartService(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity, id string) {
	id, ok := cleanID(id, true)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	body := startServiceBody{}
	if r.ContentLength != 0 {
		if err := parseBody(r, &body); err != nil {
			writeBodyError(w, err)
			return
		}
	}
	res, err := h.opts.Execute.StartService(r.Context(), ident, execute.StartServiceRequest{
		Tool:      id,
		Name:      body.Name,
		Params:    body.Params,
		Tags:      body.Tags,
		Resources: body.Resources,
	})
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// startServiceID extracts the tool id from /api/tools/<id>/start.
func startServiceID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/tools/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/start")
	return cleanID(id, ok)
}

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
