package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
)

// This file is the hand-off surface a tool's own page uses: one submission
// produces one instance, and the page then polls its state, streams its log,
// lists its artifacts and can cancel it. The page owns the presentation; these
// endpoints are the stable JSON spelling of the execution facts.
//
//	GET /api/jobs/<id>            → state JSON (the source of truth)
//	GET /api/jobs/<id>/artifacts  → declared outputs
//	GET /api/jobs/<id>/logs       → tail / SSE (see logs.go)
//	POST /api/jobs/<id>/cancel    → idempotent stop (see execute.go)

// jobLinks are the relative endpoints a UI follows from a status answer, so it
// never has to build paths (or guess which of them exist).
type jobLinks struct {
	Logs      string `json:"logs"`
	Artifacts string `json:"artifacts"`
	Cancel    string `json:"cancel"`
}

// jobStatus is the status answer. InstanceView is embedded so its fields stay
// the single wire contract for an instance; links is the only addition.
type jobStatus struct {
	inspect.InstanceView
	Links jobLinks `json:"links"`
}

// handleJobStatus returns one instance's state.
func (h *Handler) handleJobStatus(w http.ResponseWriter, r *http.Request, username, id string) {
	view, candidates, err := h.reader().Instance(username, id)
	if err != nil {
		writeInstanceLookupError(w, err, candidates)
		return
	}
	writeJSON(w, 200, jobStatus{
		InstanceView: *view,
		Links: jobLinks{
			Logs:      "/api/jobs/" + view.ID + "/logs",
			Artifacts: "/api/jobs/" + view.ID + "/artifacts",
			Cancel:    "/api/jobs/" + view.ID + "/cancel",
		},
	})
}

// handleJobArtifacts lists the outputs a task declared when it was submitted.
//
// It is not a directory walk: the answer is exactly what the submitter asked
// for, resolved through the instance's own mount table (so a path that escapes
// its mount is reported, not opened).
func (h *Handler) handleJobArtifacts(w http.ResponseWriter, r *http.Request, username, id string) {
	artifacts, err := h.reader().Artifacts(username, id)
	if err != nil {
		writeInstanceLookupError(w, err, nil)
		return
	}
	writeJSON(w, 200, map[string]any{"artifacts": artifacts})
}

// writeInstanceLookupError spells the read side's error classes for the status
// endpoints, including the "ambiguous suffix" answer (400 + candidates), which
// ErrorStatus alone would flatten to a bare message.
func writeInstanceLookupError(w http.ResponseWriter, err error, candidates []string) {
	body := map[string]any{"error": err.Error()}
	if len(candidates) > 0 {
		body["candidates"] = candidates
	}
	writeJSON(w, ErrorStatus(err), body)
}

// jobStatusID extracts the instance id from /api/jobs/<id>.
//
// Like the log endpoint, the shape is fixed: an id is exactly one segment, so
// `/api/jobs/<id>/anything-else` is a 404 rather than a lookup with a slash in
// its needle.
func jobStatusID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/jobs/")
	if !ok || rest == "" {
		return "", false
	}
	return cleanID(rest, true)
}

// jobArtifactsID extracts the instance id from /api/jobs/<id>/artifacts.
func jobArtifactsID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/jobs/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/artifacts")
	return cleanID(id, ok)
}
