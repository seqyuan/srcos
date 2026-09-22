package flow

import (
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/tool"
)

func TestParseSamples(t *testing.T) {
	// CSV, TSV and Excel's BOM must all work: these tables are written by hand,
	// by Excel and by pandas, and none of them agree on the details.
	cases := map[string]struct {
		text    string
		columns []string
		rows    int
		firstID string
	}{
		"csv": {
			text:    "sample_id,fastq_dir\nS001,/raw/S001\nS002,/raw/S002\n",
			columns: []string{"sample_id", "fastq_dir"}, rows: 2, firstID: "S001",
		},
		"tsv": {
			text:    "sample_id\tfastq_dir\nS001\t/raw/S001\n",
			columns: []string{"sample_id", "fastq_dir"}, rows: 1, firstID: "S001",
		},
		"bom and trailing newline": {
			text:    "\ufeffsample_id,workdir\nS001,/proj/S001\n\n",
			columns: []string{"sample_id", "workdir"}, rows: 1, firstID: "S001",
		},
		"quoted value with a comma": {
			text:    "sample_id,note\nS001,\"a, b\"\n",
			columns: []string{"sample_id", "note"}, rows: 1, firstID: "S001",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, err := ParseSamples(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(s.Columns, ",") != strings.Join(tc.columns, ",") {
				t.Fatalf("columns = %v, want %v", s.Columns, tc.columns)
			}
			if len(s.Rows) != tc.rows {
				t.Fatalf("rows = %d, want %d", len(s.Rows), tc.rows)
			}
			if s.Rows[0].ID != tc.firstID {
				t.Fatalf("first id = %q", s.Rows[0].ID)
			}
		})
	}
}

func TestParseSamplesRejectsMalformedTables(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"header only":       "sample_id,fastq_dir\n",
		"duplicate column":  "sample_id,sample_id\nS1,/x\n",
		"empty column name": "sample_id,\nS1,/x\n",
		"ragged row":        "sample_id,fastq_dir\nS1\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSamples(text); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

func TestCheckColumns(t *testing.T) {
	f := &Flow{Expose: []Expose{
		{Node: "count", Input: "fastq_dir", From: "sample.fastq_dir"},
		{Node: "count", Input: "sample_id", From: "sample.sample_id"},
	}}
	s, err := ParseSamples("sample_id,fastq_dir\nS1,/raw/S1\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.CheckColumns(s); err != nil {
		t.Fatalf("the table has both columns: %v", err)
	}

	missing, err := ParseSamples("sample_id\nS1\n")
	if err != nil {
		t.Fatal(err)
	}
	err = f.CheckColumns(missing)
	if err == nil || !strings.Contains(err.Error(), "fastq_dir") {
		t.Fatalf("error = %v, want the missing column named", err)
	}
}

// The planner is the contract in action: every parameter value has a source,
// and the sources are the four the spec defines.
func TestPlanResolvesEveryParameter(t *testing.T) {
	f := &Flow{
		SchemaVersion: 1, ID: "scrna", Version: "0.1.0", Name: "scrna",
		Nodes: []Node{
			{ID: "count", Tool: "count"},
			{ID: "qc", Tool: "qc", DependsOn: []string{"count"}},
		},
		Bindings: []Binding{{From: "count.outputs.outs", To: "qc.inputs.input_dir"}},
		Expose: []Expose{
			{Node: "count", Input: "sample_id", From: "sample.sample_id"},
			{Node: "count", Input: "outdir", From: "output.outs"},
			{Node: "count", Input: "workers", From: "sample.workers"},
			{Node: "count", Input: "transcriptome", From: "user"},
			{Node: "qc", Input: "outdir", From: "output.clean"},
			{Node: "qc", Input: "min_genes", From: "user"},
		},
	}
	count := &tool.Tool{ID: "count", Version: "1.0.0", Kind: tool.KindTask, Interface: tool.Interface{
		Inputs: []tool.Input{
			{Name: "sample_id", Type: tool.TypeString, Required: true},
			{Name: "outdir", Type: tool.TypeDirectory, Required: true},
			{Name: "workers", Type: tool.TypeInt, Default: 1},
			{Name: "transcriptome", Type: tool.TypeString, Required: true},
		},
		Outputs: []tool.Output{{Name: "outs", Type: tool.OutDirectory}},
	}}
	qcTool := &tool.Tool{ID: "qc", Version: "1.0.0", Kind: tool.KindTask, Interface: tool.Interface{
		Inputs: []tool.Input{
			{Name: "input_dir", Type: tool.TypeDirectory, Required: true},
			{Name: "outdir", Type: tool.TypeDirectory, Required: true},
			{Name: "min_genes", Type: tool.TypeInt, Required: true},
		},
		Outputs: []tool.Output{{Name: "clean", Type: tool.OutDirectory}},
	}}
	manifests := map[string]*tool.Tool{"count": count, "qc": qcTool}

	samples, err := ParseSamples("sample_id,workers\nS001,4\nS002,8\n")
	if err != nil {
		t.Fatal(err)
	}
	units, err := Plan(PlanInput{
		Flow: f, Samples: samples, RunID: "run-1", Manifests: manifests,
		Params: map[string]string{"transcriptome": "GRCh38", "min_genes": "200"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(units) != 4 {
		t.Fatalf("units = %d, want 2 nodes × 2 samples", len(units))
	}

	// Dependency order: every count unit precedes every qc unit.
	if ids := NodeIDs(units); strings.Join(ids, ",") != "count,qc" {
		t.Fatalf("node order = %v", ids)
	}

	first := units[0]
	if first.Node != "count" || first.Segment != "s01-s001" {
		t.Fatalf("first unit = %+v", first)
	}
	if first.Params["sample_id"] != "S001" {
		t.Fatalf("from sample: %v", first.Params)
	}
	if first.Params["workers"] != 4 {
		t.Fatalf("a table value must be coerced to the declared type: %#v", first.Params["workers"])
	}
	if first.Params["transcriptome"] != "GRCh38" {
		t.Fatalf("from user: %v", first.Params)
	}
	wantOut := "s01-s001/outs"
	if got, _ := first.Params["outdir"].(string); !strings.HasSuffix(got, wantOut) {
		t.Fatalf("from output: %q, want it to end with %q", got, wantOut)
	}
	// Outputs are the unit's own declared outputs, one path per output name.
	if len(first.Outputs) != 1 || first.Outputs[0] != first.Params["outdir"] {
		t.Fatalf("outputs = %v (outdir %v)", first.Outputs, first.Params["outdir"])
	}
	// Tags make the generated jobs findable.
	if first.Tags["run"] != "run-1" || first.Tags["node"] != "count" || first.Tags["sample"] != "S001" {
		t.Fatalf("tags = %v", first.Tags)
	}

	// The qc unit's input is the *count* unit's output for the same sample.
	qcUnit := units[2]
	if qcUnit.Node != "qc" || qcUnit.Segment != "s01-s001" {
		t.Fatalf("qc unit = %+v", qcUnit)
	}
	if qcUnit.Params["input_dir"] != first.Params["outdir"] {
		t.Fatalf("the wire must carry the upstream's output path:\n  up %v\n  down %v",
			first.Params["outdir"], qcUnit.Params["input_dir"])
	}
	// A user-supplied value reaches the tool, coerced.
	if qcUnit.Params["min_genes"] != 200 {
		t.Fatalf("min_genes = %#v, want the integer 200", qcUnit.Params["min_genes"])
	}
	// A parameter nobody wires stays out of the job: the tool's own default
	// applies (job.Validate is the authority on whether that is enough).
	if _, present := first.Params["nonexistent"]; present {
		t.Fatalf("unexpected param: %v", first.Params)
	}
}

func TestPlanRefusesMissingUserValues(t *testing.T) {
	f := &Flow{
		SchemaVersion: 1, ID: "scrna", Version: "0.1.0", Name: "scrna",
		Nodes: []Node{{ID: "count", Tool: "count"}},
		Expose: []Expose{
			{Node: "count", Input: "sample_id", From: "sample.sample_id"},
			{Node: "count", Input: "outdir", From: "output.outs"},
			{Node: "count", Input: "transcriptome", From: "user"},
		},
	}
	// A required input the flow exposes as a user value: the planner must say
	// what to pass rather than submitting a job that fails the tool's check.
	count := counting()
	count.Interface.Inputs[2].Required = true
	count.Interface.Inputs[2].Default = nil
	manifests := map[string]*tool.Tool{"count": count}
	samples, err := ParseSamples("sample_id\nS001\n")
	if err != nil {
		t.Fatal(err)
	}

	_, err = Plan(PlanInput{Flow: f, Samples: samples, RunID: "run-1", Manifests: manifests})
	if err == nil || !strings.Contains(err.Error(), "--param transcriptome") {
		t.Fatalf("error = %v, want it to say what to pass", err)
	}

	// The node-qualified spelling works too, for an input exposed by two nodes.
	if _, err := Plan(PlanInput{Flow: f, Samples: samples, RunID: "run-1", Manifests: manifests,
		Params: map[string]string{"count.transcriptome": "GRCh38"}}); err != nil {
		t.Fatalf("a node-qualified param must be accepted: %v", err)
	}
}

func TestPlanRejectsBadTablesAndIds(t *testing.T) {
	f := &Flow{
		SchemaVersion: 1, ID: "scrna", Version: "0.1.0", Name: "scrna",
		Nodes: []Node{{ID: "count", Tool: "count"}},
		Expose: []Expose{
			{Node: "count", Input: "sample_id", From: "sample.sample_id"},
			{Node: "count", Input: "outdir", From: "output.outs"},
		},
	}
	manifests := map[string]*tool.Tool{"count": counting()}

	// A column the flow reads but the table lacks.
	samples, err := ParseSamples("other\nx\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(PlanInput{Flow: f, Samples: samples, RunID: "run-1", Manifests: manifests}); err == nil {
		t.Fatal("a missing column must be an error")
	}

	// An id that would not survive as a path segment.
	ok, err := ParseSamples("sample_id\nS001\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(PlanInput{Flow: f, Samples: ok, RunID: "../escape", Manifests: manifests}); err == nil {
		t.Fatal("a run id with a path separator must be refused")
	}
}

// A sample id comes from a user's table and may contain anything; the segment it
// becomes must never leave its directory.
func TestSampleSegmentIsSafeAndUnique(t *testing.T) {
	hostile := []string{"../../etc/passwd", "a b", "s/1", ".", "Ünïcode", ""}
	for i, id := range hostile {
		segment := SampleSegment(i, id)
		if strings.ContainsAny(segment, "/ ") || strings.HasPrefix(segment, ".") {
			t.Errorf("SampleSegment(%q) = %q is not a safe path segment", id, segment)
		}
		if !strings.HasPrefix(segment, "s0") {
			t.Errorf("SampleSegment(%q) = %q must start with the row number", id, segment)
		}
	}
	// Two rows with the same sample name still get distinct directories: one
	// unit's outputs must never overwrite another's.
	if SampleSegment(0, "S1") == SampleSegment(1, "S1") {
		t.Fatal("two rows sharing a sample name must still get distinct segments")
	}
}

func TestRunIDsAreValidAndUnique(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewRunID("scrna", now)
		if !ValidRunID(id) {
			t.Fatalf("generated id %q is not a valid path segment", id)
		}
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
	if ValidRunID("../../x") || ValidRunID("") {
		t.Fatal("invalid ids must be refused")
	}
}

func TestLayoutPaths(t *testing.T) {
	// The host and sandbox views must describe the same place.
	sandbox := SandboxSampleDir("run-1", "count", "s01-s001")
	if sandbox != "/flow/runs/run-1/nodes/count/s01-s001" {
		t.Fatalf("sandbox sample dir = %q", sandbox)
	}
	if got := SandboxOutput("run-1", "count", "s01-s001", "outs"); got != sandbox+"/outs" {
		t.Fatalf("sandbox output = %q", got)
	}
	host := SampleDir("/data", "alice", "run-1", "count", "s01-s001")
	if host != "/data/flows/alice/runs/run-1/nodes/count/s01-s001" {
		t.Fatalf("host sample dir = %q", host)
	}
	if got := SignPath("/data", "alice", "run-1", "count"); got != "/data/flows/alice/runs/run-1/nodes/count/.sign" {
		t.Fatalf("sign path = %q", got)
	}
}
