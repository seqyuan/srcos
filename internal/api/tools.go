package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the tool-facing HTTP surface: the tool catalogue, the
// machine-readable interface, and the path browser behind the
// srcos-path-picker primitive control.
//
// The answers themselves are not computed here — they come from package
// inspect, which the MCP server reads too (ADR-018: one implementation, two
// front-ends). What lives here is the HTTP spelling of them: status codes,
// JSON shapes, and the query-string grammar.

// reader builds the read-only view for this handler's deployment.
func (h *Handler) reader() *inspect.Reader {
	return &inspect.Reader{
		ConfigDir: h.configDir(),
		ToolsDir:  h.opts.ToolsDir,
		Storages:  h.opts.Storages,
		Grants:    h.opts.Grants,
	}
}

// errorStatus maps the read side's error classes onto HTTP.
//
// The mapping is explicit rather than "500 by default" so a new read answer
// cannot quietly turn "not permitted" into a server error: every class has a
// decided spelling here, and the fallback is the honest one (500).
func errorStatus(err error) int {
	switch {
	case errors.Is(err, inspect.ErrBadRequest):
		return http.StatusBadRequest
	case errors.Is(err, inspect.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, inspect.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, inspect.ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// handleListTools returns the catalogue.
func (h *Handler) handleListTools(w http.ResponseWriter, username string) {
	views, err := h.reader().Tools(username)
	if err != nil {
		writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	// The catalogue entry is a summary: the interface is what describe is for,
	// and a picker that wants the storages asks for one tool (ADR-018).
	out := make([]map[string]any, 0, len(views))
	for _, t := range views {
		out = append(out, map[string]any{
			"id":          t.ID,
			"version":     t.Version,
			"name":        t.Name,
			"description": t.Description,
			"kind":        t.Kind,
			"backend":     t.Backend,
			"interface":   t.Interface,
		})
	}
	writeJSON(w, 200, map[string]any{"tools": out})
}

// handleDescribeTool returns one tool's full interface.
//
// This is the contract made machine-readable: a tool's own UI reads it to
// render its form, an agent reads it to build a tool call, and the canvas reads
// it to validate a wire. One source, three consumers (ADR-018).
func (h *Handler) handleDescribeTool(w http.ResponseWriter, username, id string) {
	t, err := h.reader().Tool(username, id)
	if err != nil {
		// Unknown and unauthorized are one status here as well as one class:
		// the caller must not be able to enumerate other people's tools.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"id":          t.ID,
		"version":     t.Version,
		"name":        t.Name,
		"description": t.Description,
		"kind":        t.Kind,
		"backend":     t.Backend,
		"sandbox":     t.Sandbox,
		"entry":       t.Entry,
		"interface":   t.Interface,
		"resources":   t.Resources,
		// The storages a path parameter may select from, so a UI can render the
		// root switcher without a second call.
		"storages": t.Storages,
	})
}

// handlePaths lists one directory inside a storage, as a sandbox path.
//
// The storage is derived from the tool and input the caller names, never from a
// free-form storage id parameter alone. That is the ADR-020 closure enforced at
// the API edge: a caller cannot enumerate a storage a tool did not declare, so
// "what the UI offers" and "what the sandbox mounts" cannot drift.
func (h *Handler) handlePaths(w http.ResponseWriter, r *http.Request, username string) {
	if h.opts.Storages == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no StorageProvider is configured on this host"})
		return
	}
	q := r.URL.Query()
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	listing, err := h.reader().Paths(r.Context(), username, inspect.PathRequest{
		Tool:    strings.TrimSpace(q.Get("tool")),
		Input:   strings.TrimSpace(q.Get("input")),
		Storage: strings.TrimSpace(q.Get("storage")),
		Path:    strings.TrimSpace(q.Get("path")),
		Select:  strings.TrimSpace(q.Get("select")),
		Limit:   limit,
	})
	if err != nil {
		writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, listing)
}

// storagesFor resolves a tool's declared storages for the form renderer.
func (h *Handler) storagesFor(t *tool.Tool) []storage.Storage {
	return h.reader().StorageDefs(t)
}

// handleToolForm renders the generated fallback form for a tool.
//
// This is ADR-017's necessary completion: a tool that ships only work.sh +
// interface is still usable, because SRCOS renders a form from the signature.
// A tool that wants a prettier UI replaces this page, and the signature stays
// the same.
func (h *Handler) handleToolForm(w http.ResponseWriter, r *http.Request, username, id string) {
	if h.opts.RenderToolForm == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tool pages are not enabled"})
		return
	}
	t, err := h.reader().VisibleManifest(username, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(h.opts.RenderToolForm(username, t, h.storagesFor(t))))
}
