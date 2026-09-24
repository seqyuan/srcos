package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/flow"
)

// The canvas's two extra pieces (ADR-023) live on the same admin surface as the
// flow, and are read/written separately: a drag must never be able to make a
// valid flow look broken, and the expose suggestion must come from the server's
// rule rather than a second copy in JavaScript.

func TestFlowEditorViewCarriesLayoutAndSuggestions(t *testing.T) {
	h := newFlowHarness(t)

	rec := h.asAdmin("GET", "/api/admin/flows/scrna", "")
	if rec.Code != 200 {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	var view struct {
		Flow            map[string]any `json:"flow"`
		Layout          flow.Layout    `json:"layout"`
		SuggestedExpose []struct {
			Node  string `json:"node"`
			Input string `json:"input"`
			From  string `json:"from"`
		} `json:"suggestedExpose"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	// No layout saved yet: an empty map, not a missing field (the canvas would
	// otherwise have to guard against undefined).
	if view.Layout.Nodes == nil || len(view.Layout.Nodes) != 0 {
		t.Fatalf("layout = %+v", view.Layout)
	}
	// The fixture's flow is complete, so nothing is suggested.
	if len(view.SuggestedExpose) != 0 {
		t.Fatalf("suggested = %+v", view.SuggestedExpose)
	}

	// Remove an expose entry: it comes back as a suggestion, with the
	// conventional sample column.
	writeFlowFixture(t, h.opts.FlowsDir, "missing", `schemaVersion: 1
id: missing
version: 0.1.0
name: "缺 expose"
nodes:
  - {id: count, tool: count}
expose:
  - {node: count, input: fastq_dir, from: sample.fastq_dir}
`)
	rec = h.asAdmin("GET", "/api/admin/flows/missing", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.SuggestedExpose) != 1 {
		t.Fatalf("suggested = %+v", view.SuggestedExpose)
	}
	got := view.SuggestedExpose[0]
	if got.Node != "count" || got.Input != "sample_id" || got.From != "sample.sample_id" {
		t.Fatalf("suggestion = %+v", got)
	}
}

func TestFlowLayoutRoundTripThroughTheAPI(t *testing.T) {
	h := newFlowHarness(t)

	body := `{"nodes":{"count":{"x":123.4,"y":56.7},"ghost":{"x":1,"y":1}}}`
	rec := h.asAdmin("PUT", "/api/admin/flows/scrna/layout", body)
	if rec.Code != 200 {
		t.Fatalf("put layout = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "123.4") {
		t.Fatalf("the saved layout was not echoed: %s", rec.Body)
	}

	// It is its own file, next to flow.yaml — and flow.yaml is untouched.
	layoutPath := flow.LayoutPath(h.opts.FlowsDir, "scrna")
	if _, err := os.Stat(layoutPath); err != nil {
		t.Fatalf("layout.yaml was not written: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(h.opts.FlowsDir, "scrna", "flow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "123.4") || strings.Contains(string(raw), "layout") {
		t.Fatalf("the layout leaked into the contract:\n%s", raw)
	}

	// A node that is not in the flow is dropped (a hand-edit can remove nodes).
	data, err := os.ReadFile(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghost") {
		t.Fatalf("a stale node survived: %s", data)
	}

	// And the editor view serves it back.
	rec = h.asAdmin("GET", "/api/admin/flows/scrna", "")
	var view struct {
		Layout flow.Layout `json:"layout"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if p := view.Layout.Nodes["count"]; p.X != 123.4 || p.Y != 56.7 {
		t.Fatalf("layout = %+v", view.Layout)
	}
}

func TestFlowLayoutRefusesWhatItCannotDraw(t *testing.T) {
	h := newFlowHarness(t)

	// A bad flow id never reaches the filesystem.
	rec := h.asAdmin("PUT", "/api/admin/flows/..%2Fetc/layout", `{"nodes":{}}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Fatalf("bad id = %d %s", rec.Code, rec.Body)
	}
	// An unknown flow is 404.
	rec = h.asAdmin("PUT", "/api/admin/flows/nope/layout", `{"nodes":{}}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown flow = %d %s", rec.Code, rec.Body)
	}
	// A position that is not a number is refused rather than written.
	rec = h.asAdmin("PUT", "/api/admin/flows/scrna/layout", `{"nodes":{"count":{"x":"left","y":0}}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric coordinate = %d %s", rec.Code, rec.Body)
	}
}

// The layout surface is admin-only, like the rest of /api/admin/*.
func TestFlowLayoutIsAdminOnly(t *testing.T) {
	h := newFlowHarness(t)
	rec := h.as("GET", "/api/admin/flows/scrna", "", "alice")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin get = %d", rec.Code)
	}
	rec = h.as("PUT", "/api/admin/flows/scrna/layout", `{"nodes":{}}`, "alice")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin layout write = %d", rec.Code)
	}
}
