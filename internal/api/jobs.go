package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the job-facing HTTP surface: list this user's instances, and
// accept a submission.
//
// The decision-making lives in package execute, which the MCP server calls too
// (ADR-018: one implementation, two front-ends). What lives here is the HTTP
// spelling: status codes, JSON shapes, and the query-string grammar.

// submitJobBody is the request shape for POST /api/jobs.
//
// It mirrors job.json but keeps the tool and the display name outside it: the
// tool is an addressing fact (it selects which interface validates the params)
// and the name is presentation, so neither belongs in the on-disk record where
// the sandbox could read them as instructions.
type submitJobBody struct {
	Tool      string            `json:"tool"`
	Name      string            `json:"name"`
	Params    map[string]any    `json:"params"`
	Resources *tool.Resources   `json:"resources"`
	Outputs   []string          `json:"outputs"`
	Tags      map[string]string `json:"tags"`
	// Run starts the job immediately instead of leaving it in the drop-box. The
	// default is false: submitting from a form queues work, and an operator (or
	// an agent, which cannot drain a queue) asks for a start explicitly.
	Run *bool `json:"run,omitempty"`
}

// handleSubmitJob validates a submission against the tool's interface and drops
// it into the job directory, starting it when the caller asked to.
func (h *Handler) handleSubmitJob(w http.ResponseWriter, r *http.Request, ident agenttoken.Identity) {
	var body submitJobBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	run := false
	if body.Run != nil {
		run = *body.Run
	}
	res, err := h.opts.Execute.Submit(ident, execute.SubmitRequest{
		Tool:      body.Tool,
		Name:      body.Name,
		Params:    body.Params,
		Resources: body.Resources,
		Outputs:   body.Outputs,
		Tags:      body.Tags,
		Run:       run,
	})
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// handleListJobs returns this user's instances.
//
// Scoped to the user on purpose: the instance list is a per-user view, and a
// leak here would expose another user's activity and endpoints. The
// management-side view (all users) lives behind the admin role.
func (h *Handler) handleListJobs(w http.ResponseWriter, r *http.Request, username string) {
	if h.configDir() == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no instance store is configured on this host"})
		return
	}
	views, err := h.reader().Instances(username,
		strings.TrimSpace(r.URL.Query().Get("tool")),
		strings.TrimSpace(r.URL.Query().Get("kind")))
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": views})
}

// configDir returns the directory instances live under. It is derived from the
// user registry's config directory so the two never disagree.
func (h *Handler) configDir() string {
	if h.Registry == nil {
		return h.opts.ConfigDir
	}
	return config.DirOf(h.Registry)
}
