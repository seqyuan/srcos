package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/tool"
)

// A tool's own default feeds a required input — for a plain job (job.Validate
// judges on the effective params) and therefore for a flow node too. The
// validator's own message said so long before it did so.
func TestRequiredInputWithAToolDefaultNeedsNoSource(t *testing.T) {
	withDefault := &tool.Tool{
		ID: "demo", Version: "1.0.0", Kind: tool.KindTask,
		Interface: tool.Interface{
			Inputs: []tool.Input{
				{Name: "label", Type: tool.TypeString, Required: true, Default: "hi"},
			},
		},
	}
	f := &Flow{
		SchemaVersion: 1, ID: "d", Version: "0.1.0", Name: "D",
		Nodes: []Node{{ID: "one", Tool: "demo"}},
	}
	if err := f.ValidateAgainst(toolset(map[string]*tool.Tool{"demo": withDefault})); err != nil {
		t.Fatalf("a tool default must satisfy a required input: %v", err)
	}

	// Without the default it is still a problem, with the hint that says how to
	// fix it.
	noDefault := *withDefault
	noDefault.Interface.Inputs = []tool.Input{{Name: "label", Type: tool.TypeString, Required: true}}
	if err := f.ValidateAgainst(toolset(map[string]*tool.Tool{"demo": &noDefault})); err == nil {
		t.Fatal("a required input with no source and no default must be refused")
	} else if !strings.Contains(err.Error(), "no source") {
		t.Fatalf("error = %v", err)
	}
}

func TestMissingExposeIsTheComplementOfValidation(t *testing.T) {
	manifests := map[string]*tool.Tool{"count": counting(), "qc": qc(), "report": report()}
	resolve := toolset(manifests)

	// The valid fixture is complete: nothing is missing.
	if got := MissingExpose(validFlow(), resolve); len(got) != 0 {
		t.Fatalf("a valid flow should need nothing, got %+v", got)
	}

	// Drop one expose entry: exactly that input comes back, suggesting the
	// conventional sample column.
	f := validFlow()
	f.Expose = f.Expose[:1]
	got := MissingExpose(f, resolve)
	if len(got) != 1 || got[0].Node != "count" || got[0].Input != "sample_id" {
		t.Fatalf("missing = %+v", got)
	}
	if got[0].From != "sample.sample_id" {
		t.Errorf("from = %q, want the conventional sample column", got[0].From)
	}

	// And applying the suggestion makes the flow valid — that is the whole
	// point of deriving it next to the rule.
	f.Expose = append(f.Expose, got...)
	if err := f.ValidateAgainst(resolve); err != nil {
		t.Fatalf("applying the suggestion should validate: %v", err)
	}

	// A bound input is not "missing" even when it is also required and unexposed.
	bound := &Flow{
		SchemaVersion: 1, ID: "b", Version: "0.1.0", Name: "B",
		Nodes: []Node{
			{ID: "one", Tool: "count"},
			{ID: "two", Tool: "qc", DependsOn: []string{"one"}},
		},
		Bindings: []Binding{{From: "one.outputs.outs", To: "two.inputs.input_dir"}},
		Expose: []Expose{
			{Node: "one", Input: "fastq_dir", From: "sample.fastq_dir"},
			{Node: "one", Input: "sample_id", From: "sample.sample_id"},
		},
	}
	if err := bound.ValidateAgainst(resolve); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}
	for _, m := range MissingExpose(bound, resolve) {
		if m.Node == "two" {
			t.Errorf("a bound input was reported as missing: %+v", m)
		}
	}
}

func TestLayoutRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := "pipe"
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}

	// No file yet: an empty layout, not an error (the canvas derives one).
	if got := LoadLayout(dir, id, []string{"a"}); len(got.Nodes) != 0 {
		t.Fatalf("missing layout = %+v", got)
	}

	want := Layout{Nodes: map[string]Point{
		"a":    {X: 12, Y: 34},
		"b":    {X: 400, Y: 90},
		"gone": {X: 1, Y: 1}, // not a node of the flow: must not be stored
	}}
	if err := SaveLayout(dir, id, want, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	got := LoadLayout(dir, id, []string{"a", "b"})
	if len(got.Nodes) != 2 || got.Nodes["a"] != (Point{12, 34}) || got.Nodes["b"] != (Point{400, 90}) {
		t.Fatalf("round trip = %+v", got.Nodes)
	}

	// The sidecar is its own file: flow.yaml is untouched by a drag.
	if _, err := os.Stat(filepath.Join(dir, id, "layout.yaml")); err != nil {
		t.Fatalf("layout.yaml was not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, id, "flow.yaml")); !os.IsNotExist(err) {
		t.Error("the layout must not be written into flow.yaml")
	}

	// A node that no longer exists is hidden on load even if the file still
	// lists it (a flow edited by hand outside the console).
	stale := Layout{Nodes: map[string]Point{"a": {X: 5, Y: 6}}}
	if err := SaveLayout(dir, id, stale, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if got := LoadLayout(dir, id, []string{"other"}); len(got.Nodes) != 0 {
		t.Fatalf("stale coordinates leaked: %+v", got.Nodes)
	}
}

// A broken sidecar degrades to the derived layout instead of failing the page.
func TestLayoutToleratesCorruption(t *testing.T) {
	dir := t.TempDir()
	id := "pipe"
	if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(LayoutPath(dir, id), []byte("nodes: [this is not a map"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadLayout(dir, id, []string{"a"}); len(got.Nodes) != 0 {
		t.Fatalf("a corrupt layout must read as empty, got %+v", got)
	}
}
