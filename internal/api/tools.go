package api

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the tool-facing HTTP surface: the tool catalogue, the
// machine-readable interface, and the path browser behind the
// srcos-path-picker primitive control.
//
// Three routes, one purpose: let a UI (SRCOS's own generated form, a tool's
// self-built shiny page, or an agent over MCP) discover what a tool accepts and
// offer a valid value for a path parameter. None of them let the caller choose
// a storage freely — see handlePaths.

// pathResponse is what the picker renders from.
type pathResponse struct {
	Tool     string        `json:"tool"`
	Input    string        `json:"input"`
	Select   string        `json:"select,omitempty"`
	Storages []storageView `json:"storages"`
	Storage  string        `json:"storage"`
	Path     string        `json:"path"`
	// Parent is the enclosing directory, for an "up" control. Empty at a root.
	Parent    string          `json:"parent,omitempty"`
	Entries   []storage.Entry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

type storageView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"`
	Mode string `json:"mode"`
}

// handleListTools returns the catalogue.
func (h *Handler) handleListTools(w http.ResponseWriter, username string) {
	tools, err := h.visibleTools(username)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"id":          t.ID,
			"version":     t.Version,
			"name":        t.Name,
			"description": t.Description,
			"kind":        string(t.Kind),
			"backend":     string(t.Backend),
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
	t, err := h.findVisibleTool(username, id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"id":          t.ID,
		"version":     t.Version,
		"name":        t.Name,
		"description": t.Description,
		"kind":        string(t.Kind),
		"backend":     string(t.Backend),
		"sandbox":     string(t.Sandbox),
		"entry":       t.Entry,
		"interface":   t.Interface,
		"resources":   t.Resources,
		// The storages a path parameter may select from, so a UI can render the
		// root switcher without a second call.
		"storages": h.storageViewsFor(t),
	})
}

// handlePaths lists one directory inside a storage, as a sandbox path.
//
// The storage is derived from the tool and input the caller names, never taken
// from a free-form storage id parameter alone. That is the ADR-020 closure
// enforced at the API edge: a caller cannot enumerate a storage a tool did not
// declare, so "what the UI offers" and "what the sandbox mounts" cannot drift.
//
// The `storage=` parameter exists only to switch between the roots a single
// input declared, and is validated against that list.
func (h *Handler) handlePaths(w http.ResponseWriter, r *http.Request, username string) {
	if h.opts.Storages == nil {
		writeJSON(w, 503, map[string]string{"error": "no StorageProvider is configured on this host"})
		return
	}
	q := r.URL.Query()
	toolID := strings.TrimSpace(q.Get("tool"))
	inputName := strings.TrimSpace(q.Get("input"))
	if toolID == "" || inputName == "" {
		writeJSON(w, 400, map[string]string{"error": "both `tool` and `input` are required"})
		return
	}

	t, err := h.findVisibleTool(username, toolID)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	in, ok := findInput(t, inputName)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": fmt.Sprintf("tool %s has no input %q", t.ID, inputName)})
		return
	}
	ids := fromIDs(in.From)
	if len(ids) == 0 {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf(
			"input %s is not storage-bound: only type path (or file/directory with a `from`) can be browsed", inputName)})
		return
	}

	// The input's own declaration is the allowlist. `storage=` may narrow it.
	chosen := strings.TrimSpace(q.Get("storage"))
	if chosen == "" {
		chosen = ids[0]
	}
	if !contains(ids, chosen) {
		writeJSON(w, 403, map[string]string{"error": fmt.Sprintf(
			"storage %q is not declared by tool %s input %s (allowed: %v)", chosen, t.ID, inputName, ids)})
		return
	}

	sandboxPath := strings.TrimSpace(q.Get("path"))
	sel := storage.Select(strings.TrimSpace(q.Get("select")))
	if sel == "" {
		sel = selectOf(in)
	}
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	res := pathResponse{
		Tool:     t.ID,
		Input:    inputName,
		Select:   string(sel),
		Storage:  chosen,
		Storages: h.storageViews(t, ids),
	}
	for _, s := range h.storageViews(t, ids) {
		if s.ID == chosen && sandboxPath == "" {
			sandboxPath = s.Root
		}
	}
	res.Path = sandboxPath

	listing, err := h.opts.Storages.Listing(r.Context(), chosen, sandboxPath, sel, limit)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	res.Path = listing.Path
	res.Entries = listing.Entries
	if res.Entries == nil {
		res.Entries = []storage.Entry{}
	}
	res.Truncated = listing.Truncated
	if parent := parentOf(listing.Path); parent != "" {
		if _, err := h.opts.Storages.Resolve(chosen, parent); err == nil {
			res.Parent = parent
		}
	}
	writeJSON(w, 200, res)
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// visibleTools is the catalogue a user may see. Grant filtering lands here in
// Phase 3; until then every authenticated user sees everything, which is
// exactly the gap the authorization model closes.
func (h *Handler) visibleTools(username string) ([]*tool.Tool, error) {
	if h.opts.ToolsDir == "" {
		return nil, nil
	}
	all, err := tool.Discover(h.opts.ToolsDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return h.filterAllowed(username, all), nil
}

// filterAllowed is the single choke point for "may this user see this tool".
// Phase 3 replaces the body; keeping it a function means the call sites do not
// change when the rule arrives.
func (h *Handler) filterAllowed(username string, tools []*tool.Tool) []*tool.Tool {
	if h.opts.Grants == nil {
		return tools
	}
	allowed := make([]*tool.Tool, 0, len(tools))
	for _, t := range tools {
		if h.opts.Grants.Allowed(username, t.ID) {
			allowed = append(allowed, t)
		}
	}
	return allowed
}

func (h *Handler) findVisibleTool(username, id string) (*tool.Tool, error) {
	if h.opts.ToolsDir == "" {
		return nil, fmt.Errorf("no tool directory is configured on this host")
	}
	tools, err := h.visibleTools(username)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if t.ID == id {
			return t, nil
		}
	}
	// Distinguish "does not exist" from "not allowed for you": the second is a
	// grant decision the user can ask an admin about.
	if _, err := tool.Find(h.opts.ToolsDir, id); err == nil {
		return nil, fmt.Errorf("tool %s is not authorized for you", id)
	}
	return nil, fmt.Errorf("unknown tool %s", id)
}

func (h *Handler) storageViews(t *tool.Tool, ids []string) []storageView {
	if h.opts.Storages == nil {
		return nil
	}
	out := make([]storageView, 0, len(ids))
	for _, id := range ids {
		s, ok := h.opts.Storages.Get(id)
		if !ok {
			continue
		}
		out = append(out, storageView{ID: s.ID, Name: s.Name, Root: s.SandboxPath, Mode: string(s.Mode)})
	}
	return out
}

func (h *Handler) storageViewsFor(t *tool.Tool) []storageView {
	if h.opts.Storages == nil || len(t.RequiresStorages) == 0 {
		return nil
	}
	return h.storageViews(t, t.RequiresStorages)
}

// storagesFor resolves a tool's declared storages for the form renderer.
func (h *Handler) storagesFor(t *tool.Tool) []storage.Storage {
	if h.opts.Storages == nil || len(t.RequiresStorages) == 0 {
		return nil
	}
	out := make([]storage.Storage, 0, len(t.RequiresStorages))
	for _, s := range h.opts.Storages.List() {
		for _, id := range t.RequiresStorages {
			if s.ID == id {
				out = append(out, s)
			}
		}
	}
	return out
}

func findInput(t *tool.Tool, name string) (tool.Input, bool) {
	for _, in := range t.Interface.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return tool.Input{}, false
}

// fromIDs splits an input's `from` field into storage ids.
func fromIDs(from string) []string {
	var out []string
	for _, p := range strings.Split(from, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// selectOf maps an input's type and select onto a listing filter, so a
// directory-typed parameter can never be handed a file.
func selectOf(in tool.Input) storage.Select {
	switch in.Type {
	case tool.TypeFile:
		return storage.SelectFile
	case tool.TypeDirectory, tool.TypeDirPath:
		return storage.SelectDirectory
	case tool.TypePath:
		switch in.Select {
		case "file":
			return storage.SelectFile
		case "directory":
			return storage.SelectDirectory
		}
	}
	return storage.SelectAny
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// parentOf returns the enclosing sandbox path, or "" at a mount root.
func parentOf(p string) string {
	p = strings.TrimSuffix(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// handleToolForm renders the generated fallback form for a tool.
//
// This is ADR-017's necessary completion: a tool that ships only work.sh +
// interface is still usable, because SRCOS renders a form from the signature.
// A tool that wants a prettier UI replaces this page, and the signature stays
// the same.
func (h *Handler) handleToolForm(w http.ResponseWriter, r *http.Request, username, id string) {
	if h.opts.RenderToolForm == nil {
		writeJSON(w, 404, map[string]string{"error": "tool pages are not enabled"})
		return
	}
	t, err := h.findVisibleTool(username, id)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(h.opts.RenderToolForm(username, t, h.storagesFor(t))))
}
