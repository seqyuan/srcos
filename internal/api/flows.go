package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the flow surface of the admin API: the canvas in the console
// reads a flow, checks a candidate while the administrator draws, and saves it.
//
// Two properties make it safe to put a graphical editor in front of a contract:
//
//   - **The server validates, the client only previews.** Every save goes
//     through the same flow.ValidateAgainst the CLI uses, so a flow drawn with a
//     mouse and one typed in an editor are held to exactly the same rules — and
//     the canvas asks the server to validate a *candidate* rather than
//     reimplementing the type rules in JavaScript, where they would drift.
//   - **A flow id is a directory name and nothing else.** The path is derived
//     from a validated slug, never taken from the request body, so no request can
//     address a file outside the flows directory.

// flowBody is the wire shape of a flow. It is the Flow struct itself: the
// contract and the wire format are the same document, which is what lets the
// canvas round-trip a flow without a translation layer that could lose fields.
type flowBody = flow.Flow

// adminFlowView is one flow in the list.
type adminFlowView struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Nodes       int      `json:"nodes"`
	Layers      int      `json:"layers"`
	SampleCols  []string `json:"sampleColumns,omitempty"`
	Valid       bool     `json:"valid"`
	// Problem is the validation error, when the flow does not validate: an
	// administrator opening the console should see which flow is broken.
	Problem string `json:"problem,omitempty"`
}

// flowEditorView is everything the canvas needs for one flow: the flow itself,
// the tool catalogue (with interfaces, for drawing a wire and editing a param),
// and the current validation verdict.
type flowEditorView struct {
	Flow    flowBody        `json:"flow"`
	Tools   []adminToolView `json:"tools"`
	Valid   bool            `json:"valid"`
	Problem string          `json:"problem,omitempty"`
	// Layout is the flow's hand-placed node coordinates (ADR-023): a sidecar
	// file, not part of the contract, empty when nothing has been placed yet.
	Layout flow.Layout `json:"layout"`
	// SuggestedExpose names the required inputs nothing feeds yet, so the canvas
	// can offer to fill them in with one click. Derived by the same rule
	// validation applies (internal/flow), never by the browser.
	SuggestedExpose []flow.Expose `json:"suggestedExpose,omitempty"`
}

// adminToolView is one tool as the canvas sees it: enough to offer it in a
// picker and to type-check a wire.
type adminToolView struct {
	ID        string         `json:"id"`
	Version   string         `json:"version"`
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Backend   string         `json:"backend"`
	Interface tool.Interface `json:"interface"`
	// Declared resources, for showing what a node will ask for.
	Resources tool.Resources `json:"resources"`
}

func (h *Handler) adminFlows(w http.ResponseWriter, r *http.Request) {
	flowsDir := h.opts.FlowsDir
	if flowsDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "no flow directory is configured on this gateway (--flows-dir)",
		})
		return
	}
	switch {
	case r.URL.Path == "/api/admin/flows" && r.Method == http.MethodGet:
		h.adminListFlows(w, flowsDir)
	case r.URL.Path == "/api/admin/flows/validate" && r.Method == http.MethodPost:
		h.adminValidateFlow(w, r, flowsDir)
	case strings.HasPrefix(r.URL.Path, "/api/admin/flows/") && r.Method == http.MethodGet:
		h.adminGetFlow(w, flowsDir, strings.TrimPrefix(r.URL.Path, "/api/admin/flows/"))
	case strings.HasPrefix(r.URL.Path, "/api/admin/flows/") && r.Method == http.MethodPut:
		rest := strings.TrimPrefix(r.URL.Path, "/api/admin/flows/")
		// The layout is a separate document (ADR-023), so it has its own write:
		// dragging a node must not run — or fail — flow validation.
		if id, ok := strings.CutSuffix(rest, "/layout"); ok {
			h.adminPutFlowLayout(w, r, flowsDir, id)
			return
		}
		h.adminPutFlow(w, r, flowsDir, rest)
	case strings.HasPrefix(r.URL.Path, "/api/admin/flows/") && r.Method == http.MethodDelete:
		http.Error(w, "deleting a flow is not supported yet", http.StatusMethodNotAllowed)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// flowPath resolves a flow id to its directory, refusing anything that is not a
// plain slug.
func flowPath(flowsDir, id string) (string, error) {
	id = strings.TrimSpace(id)
	if !flow.ValidFlowID(id) {
		return "", fmt.Errorf("invalid flow id %q", id)
	}
	return filepath.Join(flowsDir, id, "flow.yaml"), nil
}

func (h *Handler) adminListFlows(w http.ResponseWriter, flowsDir string) {
	flows, err := flow.Discover(flowsDir)
	if err != nil {
		// A directory that does not exist is "no flows yet", not an error; a
		// malformed flow is reported per entry below.
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
			h.writeFlowProblem(w, err)
		}
	}
	resolver := h.flowToolResolver()
	out := make([]adminFlowView, 0, len(flows))
	for _, f := range flows {
		v := adminFlowView{
			ID: f.ID, Version: f.Version, Name: f.Name, Description: f.Description,
			Nodes: len(f.Nodes), Layers: f.Depth(), SampleCols: f.SampleFields(),
		}
		if err := f.ValidateAgainst(resolver); err != nil {
			v.Problem = err.Error()
		} else {
			v.Valid = true
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"flows": out})
}

func (h *Handler) adminGetFlow(w http.ResponseWriter, flowsDir, id string) {
	path, err := flowPath(flowsDir, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f, err := flow.Load(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no flow %s", id)})
		return
	}
	f.Dir = ""
	resolver := h.flowToolResolver()
	view := flowEditorView{
		Flow:            *f,
		Tools:           h.flowTools(),
		Layout:          flow.LoadLayout(flowsDir, id, nodeIDs(f)),
		SuggestedExpose: flow.MissingExpose(f, resolver),
	}
	if err := f.ValidateAgainst(resolver); err != nil {
		view.Problem = err.Error()
	} else {
		view.Valid = true
	}
	writeJSON(w, 200, view)
}

// adminPutFlowLayout saves the canvas's node coordinates.
//
// It is deliberately unable to change the flow: no validation runs, because
// nothing here is part of the contract (ADR-023). The only checks are the ones
// that protect the canvas itself — a valid flow id, an existing flow, and
// coordinates a browser can actually draw.
func (h *Handler) adminPutFlowLayout(w http.ResponseWriter, r *http.Request, flowsDir, id string) {
	path, err := flowPath(flowsDir, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f, err := flow.Load(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("no flow %s", id)})
		return
	}
	var body flow.Layout
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	known := nodeIDs(f)
	if err := flow.SaveLayout(flowsDir, id, body, known); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"layout": flow.LoadLayout(flowsDir, id, known)})
}

// nodeIDs are the flow's node ids, the only keys a layout may carry.
func nodeIDs(f *flow.Flow) []string {
	out := make([]string, 0, len(f.Nodes))
	for _, n := range f.Nodes {
		out = append(out, n.ID)
	}
	return out
}

// adminValidateFlow validates a candidate without saving it.
//
// This is what makes a live canvas honest: instead of the browser deciding
// whether two ports may be wired (and drifting from the contract), it asks this
// endpoint and shows the answer.
func (h *Handler) adminValidateFlow(w http.ResponseWriter, r *http.Request, flowsDir string) {
	var body flowBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	if err := body.ValidateAgainst(h.flowToolResolver()); err != nil {
		writeJSON(w, 200, map[string]any{"valid": false, "problem": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"valid": true})
}

// adminPutFlow validates and saves a flow.
//
// Validate first, save second: a flow that does not validate must not reach the
// disk, where the scheduler would refuse to run it with a message nobody sees.
func (h *Handler) adminPutFlow(w http.ResponseWriter, r *http.Request, flowsDir, id string) {
	path, err := flowPath(flowsDir, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var body flowBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	// The id in the path wins: the directory name is the identity, and letting
	// the body disagree would leave a file whose name and content differ.
	body.ID = id
	if body.SchemaVersion == 0 {
		body.SchemaVersion = flow.SchemaVersion
	}
	if err := body.ValidateAgainst(h.flowToolResolver()); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "problem": err.Error()})
		return
	}
	body.Dir = ""
	if err := flow.SaveFlow(path, &body); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"flow": body, "valid": true})
}

// flowToolResolver resolves a node's tool the way the CLI does: exact version
// match when the flow pinned one, and an error naming the path otherwise.
func (h *Handler) flowToolResolver() func(id, version string) (*tool.Tool, error) {
	return func(id, version string) (*tool.Tool, error) {
		if h.opts.ToolsDir == "" {
			return nil, fmt.Errorf("no tool directory is configured on this gateway")
		}
		t, err := tool.Find(h.opts.ToolsDir, id)
		if err != nil {
			return nil, fmt.Errorf("unknown tool %s", id)
		}
		if version != "" && t.Version != version {
			return nil, fmt.Errorf("tool %s is version %s here, but the flow asks for %s", id, t.Version, version)
		}
		return t, nil
	}
}

// flowTools lists the tools a flow may reference, with their interfaces.
func (h *Handler) flowTools() []adminToolView {
	if h.opts.ToolsDir == "" {
		return nil
	}
	tools, err := tool.Discover(h.opts.ToolsDir)
	if err != nil {
		return nil
	}
	out := make([]adminToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, adminToolView{
			ID: t.ID, Version: t.Version, Name: t.Name, Kind: string(t.Kind),
			Backend: string(t.Backend), Interface: t.Interface, Resources: t.Resources,
		})
	}
	return out
}

// writeFlowProblem reports a flows-directory level problem (unreadable
// directory) without pretending there are no flows.
func (h *Handler) writeFlowProblem(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}
