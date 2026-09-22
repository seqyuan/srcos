package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/tool"
)

// toolset resolves manifests from an in-memory map, so the validator can be
// tested without touching a tool directory.
func toolset(manifests map[string]*tool.Tool) func(id, version string) (*tool.Tool, error) {
	return func(id, version string) (*tool.Tool, error) {
		t, ok := manifests[id]
		if !ok {
			return nil, os.ErrNotExist
		}
		if version != "" && t.Version != version {
			return nil, os.ErrInvalid
		}
		return t, nil
	}
}

// counting: one required path input, one directory output.
func counting() *tool.Tool {
	return &tool.Tool{
		ID: "count", Version: "1.2.3", Kind: tool.KindTask,
		Interface: tool.Interface{
			Inputs: []tool.Input{
				{Name: "fastq_dir", Type: tool.TypePath, From: "data", Select: "directory", Required: true},
				{Name: "sample_id", Type: tool.TypeString, Required: true},
				{Name: "transcriptome", Type: tool.TypeString},
			},
			Outputs: []tool.Output{{Name: "outs", Type: tool.OutDirectory}},
		},
	}
}

// qc: takes a directory, emits a directory and a file.
func qc() *tool.Tool {
	return &tool.Tool{
		ID: "qc", Version: "0.3.0", Kind: tool.KindTask,
		Interface: tool.Interface{
			Inputs: []tool.Input{
				{Name: "input_dir", Type: tool.TypeDirectory, Required: true},
				{Name: "label", Type: tool.TypeString},
			},
			Outputs: []tool.Output{
				{Name: "clean", Type: tool.OutDirectory},
				{Name: "metrics", Type: tool.OutFile},
			},
		},
	}
}

// report: takes a directory, emits nothing.
func report() *tool.Tool {
	return &tool.Tool{
		ID: "report", Version: "1.0.0", Kind: tool.KindTask,
		Interface: tool.Interface{
			Inputs: []tool.Input{
				{Name: "indir", Type: tool.TypeDirectory, Required: true},
				{Name: "metrics", Type: tool.TypeFile},
			},
		},
	}
}

// service is not a valid node.
func service() *tool.Tool {
	return &tool.Tool{
		ID: "web", Version: "1.0.0", Kind: tool.KindService,
		Interface: tool.Interface{},
	}
}

func validFlow() *Flow {
	return &Flow{
		SchemaVersion: 1, ID: "scrna", Version: "0.1.0", Name: "scRNA",
		Nodes: []Node{
			{ID: "count", Tool: "count@1.2.3"},
			{ID: "qc", Tool: "qc", DependsOn: []string{"count"}},
			{ID: "report", Tool: "report", DependsOn: []string{"qc"}, When: WhenAlways},
		},
		Bindings: []Binding{
			{From: "count.outputs.outs", To: "qc.inputs.input_dir"},
			{From: "qc.outputs.clean", To: "report.inputs.indir"},
			{From: "qc.outputs.metrics", To: "report.inputs.metrics"},
		},
		Expose: []Expose{
			{Node: "count", Input: "fastq_dir", From: "sample.fastq_dir"},
			{Node: "count", Input: "sample_id", From: "sample.sample_id"},
		},
	}
}

func stdTools() map[string]*tool.Tool {
	return map[string]*tool.Tool{"count": counting(), "qc": qc(), "report": report(), "web": service()}
}

func TestValidFlowPasses(t *testing.T) {
	f := validFlow()
	if err := f.ValidateAgainst(toolset(stdTools())); err != nil {
		t.Fatalf("a well-formed flow must validate: %v", err)
	}

	// The topological order is what the scheduler will use; the report node is
	// last even though its `when: always` makes it special.
	order := f.Order()
	got := []string{}
	for _, n := range order {
		got = append(got, n.ID)
	}
	if strings.Join(got, ",") != "count,qc,report" {
		t.Fatalf("order = %v", got)
	}
	if f.Depth() != 3 {
		t.Fatalf("depth = %d, want 3", f.Depth())
	}
	if fields := f.SampleFields(); strings.Join(fields, ",") != "fastq_dir,sample_id" {
		t.Fatalf("sample fields = %v", fields)
	}
}

func TestValidationCatchesEveryStructuralProblem(t *testing.T) {
	cases := map[string]func(*Flow){
		"no nodes":             func(f *Flow) { f.Nodes = nil },
		"bad id":               func(f *Flow) { f.ID = "Not Valid" },
		"no name":              func(f *Flow) { f.Name = "" },
		"bad version":          func(f *Flow) { f.Version = "one" },
		"schema version":       func(f *Flow) { f.SchemaVersion = 2 },
		"duplicate node":       func(f *Flow) { f.Nodes = append(f.Nodes, f.Nodes[0]) },
		"node id with dot":     func(f *Flow) { f.Nodes[0].ID = "c.count" },
		"self dependency":      func(f *Flow) { f.Nodes[1].DependsOn = []string{"qc"} },
		"duplicate depends_on": func(f *Flow) { f.Nodes[1].DependsOn = []string{"count", "count"} },
		"unknown upstream":     func(f *Flow) { f.Nodes[1].DependsOn = []string{"ghost"} },
		"bad when":             func(f *Flow) { f.Nodes[1].When = "maybe" },
		"retry too big":        func(f *Flow) { f.Nodes[1].Retry = &Retry{Max: MaxRetry + 1} },
		"negative retry":       func(f *Flow) { f.Nodes[1].Retry = &Retry{Max: -1} },
		"unknown tool":         func(f *Flow) { f.Nodes[1].Tool = "ghost" },
		"version mismatch":     func(f *Flow) { f.Nodes[0].Tool = "count@9.9.9" },
		"service as node":      func(f *Flow) { f.Nodes[1].Tool = "web" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFlow()
			mutate(f)
			if err := f.ValidateAgainst(toolset(stdTools())); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestValidationCatchesEveryWiringProblem(t *testing.T) {
	cases := map[string]func(*Flow){
		"from is not an output address": func(f *Flow) { f.Bindings[0].From = "count.inputs.fastq_dir" },
		"to is not an input address":    func(f *Flow) { f.Bindings[0].To = "qc.outputs.clean" },
		"garbage address":               func(f *Flow) { f.Bindings[0].From = "count.outs" },
		"unknown node in from":          func(f *Flow) { f.Bindings[0].From = "ghost.outputs.outs" },
		"unknown node in to":            func(f *Flow) { f.Bindings[0].To = "ghost.inputs.input_dir" },
		"unknown output":                func(f *Flow) { f.Bindings[0].From = "count.outputs.nope" },
		"unknown input":                 func(f *Flow) { f.Bindings[0].To = "qc.inputs.nope" },
		"two bindings for one input": func(f *Flow) {
			f.Bindings = append(f.Bindings, Binding{From: "qc.outputs.clean", To: "qc.inputs.input_dir"})
		},
		// A file output cannot fill a directory input.
		"type mismatch": func(f *Flow) { f.Bindings[2].To = "report.inputs.indir" },
		// Only paths travel between nodes.
		"output to scalar input": func(f *Flow) {
			f.Bindings = append(f.Bindings, Binding{From: "qc.outputs.clean", To: "qc.inputs.label"})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFlow()
			mutate(f)
			if err := f.ValidateAgainst(toolset(stdTools())); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestValidationCatchesExposeAndClosureProblems(t *testing.T) {
	cases := map[string]string{
		"unknown node":          "unknown node",
		"unknown input":         "has no input",
		"bad from":              "from must be",
		"duplicate expose":      "already exposed",
		"expose over a binding": "already fed by binding",
		"missing required":      "has no source",
	}
	mutate := map[string]func(*Flow){
		"unknown node":          func(f *Flow) { f.Expose[0].Node = "ghost" },
		"unknown input":         func(f *Flow) { f.Expose[0].Input = "nope" },
		"bad from":              func(f *Flow) { f.Expose[0].From = "config.Para" },
		"duplicate expose":      func(f *Flow) { f.Expose = append(f.Expose, f.Expose[0]) },
		"expose over a binding": func(f *Flow) { f.Expose = append(f.Expose, Expose{Node: "qc", Input: "input_dir", From: "user"}) },
		"missing required":      func(f *Flow) { f.Expose = f.Expose[1:] },
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			f := validFlow()
			mutate[name](f)
			err := f.ValidateAgainst(toolset(stdTools()))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to mention %q", err, want)
			}
		})
	}
}

// A required input may be satisfied by the tool's own default — that is the
// third source in the closure, and it is what makes a tool with sensible
// defaults usable in a flow without any wiring at all.
func TestRequiredInputSatisfiedByToolDefault(t *testing.T) {
	withDefault := counting()
	withDefault.Interface.Inputs[1].Required = false
	withDefault.Interface.Inputs[1].Default = "sample"

	f := &Flow{
		SchemaVersion: 1, ID: "solo", Version: "0.1.0", Name: "solo",
		Nodes: []Node{{ID: "count", Tool: "count"}},
		Expose: []Expose{
			{Node: "count", Input: "fastq_dir", From: "sample.fastq_dir"},
			// sample_id has a default, so nothing needs to provide it.
		},
	}
	if err := f.ValidateAgainst(toolset(map[string]*tool.Tool{"count": withDefault})); err != nil {
		t.Fatalf("an optional input needs no wiring: %v", err)
	}

	// But making it required without a source must fail.
	required := counting()
	required.Interface.Inputs[1].Required = true
	required.Interface.Inputs[1].Default = nil
	if err := f.ValidateAgainst(toolset(map[string]*tool.Tool{"count": required})); err == nil {
		t.Fatal("a required input with no source must fail")
	}
}

func TestCycleIsReported(t *testing.T) {
	f := &Flow{
		SchemaVersion: 1, ID: "loop", Version: "0.1.0", Name: "loop",
		Nodes: []Node{
			{ID: "a", Tool: "count", DependsOn: []string{"c"}},
			{ID: "b", Tool: "count", DependsOn: []string{"a"}},
			{ID: "c", Tool: "count", DependsOn: []string{"b"}},
		},
		Expose: []Expose{
			{Node: "a", Input: "fastq_dir", From: "sample.x"},
			{Node: "a", Input: "sample_id", From: "sample.y"},
			{Node: "b", Input: "fastq_dir", From: "sample.x"},
			{Node: "b", Input: "sample_id", From: "sample.y"},
			{Node: "c", Input: "fastq_dir", From: "sample.x"},
			{Node: "c", Input: "sample_id", From: "sample.y"},
		},
	}
	err := f.ValidateAgainst(toolset(stdTools()))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("error = %v, want a cycle report", err)
	}
	// The order terminates rather than hanging: in a cycle every node has an
	// unmet dependency, so nothing is ready and the result is empty.
	if got := f.Order(); len(got) != 0 {
		t.Fatalf("order of a cyclic flow = %d nodes, want 0", len(got))
	}
}

// A flow may use the same tool twice (that is normal: count per sample group),
// as long as the node ids differ.
func TestSameToolTwiceIsLegal(t *testing.T) {
	f := &Flow{
		SchemaVersion: 1, ID: "twice", Version: "0.1.0", Name: "twice",
		Nodes: []Node{
			{ID: "first", Tool: "count"},
			{ID: "second", Tool: "count", DependsOn: []string{"first"}},
		},
		Bindings: []Binding{{From: "first.outputs.outs", To: "second.inputs.fastq_dir"}},
		Expose: []Expose{
			{Node: "first", Input: "fastq_dir", From: "sample.x"},
			{Node: "first", Input: "sample_id", From: "sample.id"},
			{Node: "second", Input: "sample_id", From: "user"},
		},
	}
	if err := f.ValidateAgainst(toolset(stdTools())); err != nil {
		t.Fatalf("two nodes may share a tool: %v", err)
	}
}

func TestDiscoverAndFind(t *testing.T) {
	dir := t.TempDir()
	write := func(id, body string) {
		if err := os.MkdirAll(filepath.Join(dir, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, id, "flow.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("scrna", "schemaVersion: 1\nid: scrna\nversion: 0.1.0\nname: scRNA\nnodes:\n  - {id: a, tool: count}\n")
	write("mismatch", "schemaVersion: 1\nid: other\nversion: 0.1.0\nname: x\nnodes:\n  - {id: a, tool: count}\n")

	flows, err := Discover(dir)
	if err == nil {
		t.Fatal("a directory/id mismatch must be reported")
	}
	if len(flows) != 2 {
		t.Fatalf("discovered %d flows, want 2 (a bad one must not hide the good ones)", len(flows))
	}

	f, err := Find(dir, "scrna")
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != "scrna" || f.Dir == "" {
		t.Fatalf("flow = %+v", f)
	}
	if _, err := Find(dir, "nope"); err == nil {
		t.Fatal("an unknown flow id must fail")
	}
	if _, err := Find(dir, "Not Valid"); err == nil {
		t.Fatal("an invalid id must fail before touching the filesystem")
	}
	if err := f.ValidateAgainst(toolset(stdTools())); err == nil {
		// node "a" uses count, whose required inputs are exposed by nobody.
		t.Fatal("an incomplete flow must not validate")
	}
}

func TestSplitToolRef(t *testing.T) {
	cases := map[string][2]string{
		"count":         {"count", ""},
		"count@1.2.3":   {"count", "1.2.3"},
		" count@1.2.3 ": {"count", "1.2.3"},
		"count@":        {"count", ""},
	}
	for in, want := range cases {
		id, version := splitToolRef(in)
		if id != want[0] || version != want[1] {
			t.Errorf("splitToolRef(%q) = %q,%q want %q,%q", in, id, version, want[0], want[1])
		}
	}
}
