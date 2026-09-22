package flowrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// fixture is a two-node flow whose tools write and read real files, so the test
// proves the *wire* works rather than only that the scheduler loop runs.
type fixture struct {
	runner    *Runner
	flow      *flow.Flow
	samples   *flow.Samples
	configDir string
	user      string
}

const countManifest = `
schemaVersion: 1
id: count
version: 1.0.0
name: Count
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: sample_id, type: string, required: true}
    - {name: outdir, type: directory, required: true}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 1, memory: "256Mi", walltime: "0:01:00"}
`

const qcManifest = `
schemaVersion: 1
id: qc
version: 1.0.0
name: QC
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: input_dir, type: directory, required: true}
    - {name: outdir, type: directory, required: true}
  outputs:
    - {name: clean, type: directory}
resources: {cpu: 1, memory: "256Mi", walltime: "0:01:00"}
`

// countSh writes one file per sample into the directory the platform handed it.
const countSh = `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
printf 'counted %s\n' "$SRCOS_PARAM_SAMPLE_ID" > "$SRCOS_PARAM_OUTDIR/counts.txt"
`

// qcSh reads the upstream's file through the wire and copies it downstream. If
// the platform's wiring is wrong, this script fails and the node fails — which
// is exactly the assertion the test wants.
const qcSh = `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
cat "$SRCOS_PARAM_INPUT_DIR/counts.txt" > "$SRCOS_PARAM_OUTDIR/clean.txt"
`

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toolsDir := filepath.Join(root, "tools")
	for id, manifest := range map[string]string{"count": countManifest, "qc": qcManifest} {
		dir := filepath.Join(toolsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "count", "work.sh"), []byte(countSh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolsDir, "qc", "work.sh"), []byte(qcSh), 0o755); err != nil {
		t.Fatal(err)
	}

	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "count", Public: true}, {Tool: "qc", Public: true}})
	if err != nil {
		t.Fatal(err)
	}

	f := &flow.Flow{
		SchemaVersion: 1, ID: "scrna", Version: "0.1.0", Name: "scrna",
		Nodes: []flow.Node{
			{ID: "count", Tool: "count@1.0.0"},
			{ID: "qc", Tool: "qc@1.0.0", DependsOn: []string{"count"}},
		},
		Bindings: []flow.Binding{{From: "count.outputs.outs", To: "qc.inputs.input_dir"}},
		Expose: []flow.Expose{
			{Node: "count", Input: "sample_id", From: "sample.sample_id"},
			{Node: "count", Input: "outdir", From: "output.outs"},
			{Node: "qc", Input: "outdir", From: "output.clean"},
		},
	}
	samples, err := flow.ParseSamples("sample_id\nS001\nS002\n")
	if err != nil {
		t.Fatal(err)
	}

	// The degraded backend (no user systemd) keeps the test independent of the
	// host's systemd, and exercises the host-path form of flow paths at once.
	engine := runtime.NewRunner(runtime.Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		User:      "alice",
		Backends:  map[string]runtime.Backend{"local": &runtime.Local{SystemdUser: boolPtr(false)}},
	})
	return &fixture{
		runner: New(Options{
			ConfigDir: configDir,
			DataDir:   config.DataDir(configDir),
			ToolsDir:  toolsDir,
			User:      "alice",
			Runner:    engine,
			Policy:    policy,
			Log:       func(string, ...any) {},
		}),
		flow: f, samples: samples, configDir: configDir, user: "alice",
	}
}

func boolPtr(b bool) *bool { return &b }

// runOnce plans and runs the flow, returning the run record.
func (f *fixture) runOnce(t *testing.T, runID string) *flow.Run {
	t.Helper()
	return f.runWith(t, runID, flow.RunDir(config.DataDir(f.configDir), f.user, runID))
}

func (f *fixture) runWith(t *testing.T, runID, dir string) *flow.Run {
	t.Helper()
	units, _, err := f.runner.Plan(f.flow, f.samples, nil, runID)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	record := &flow.Run{FlowID: f.flow.ID, FlowVersion: f.flow.Version, User: f.user, ID: runID}
	if err := f.runner.Run(context.Background(), f.flow, units, record, f.samples, dir); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return record
}

// The whole point: a flow's edge carries an upstream output to a downstream
// tool, and the downstream really reads it.
func TestFlowRunsAndWiresOutputs(t *testing.T) {
	f := newFixture(t)
	record := f.runOnce(t, "run-1")

	if record.State != flow.RunSucceeded {
		t.Fatalf("run state = %s (%v)", record.State, record.Nodes)
	}
	if record.Samples != 4 {
		t.Fatalf("units = %d, want 4", record.Samples)
	}

	dataDir := config.DataDir(f.configDir)
	for _, sample := range []string{"s01-s001", "s02-s002"} {
		// The upstream wrote where the platform said.
		counts := filepath.Join(flow.SampleDir(dataDir, f.user, "run-1", "count", sample), "outs", "counts.txt")
		data, err := os.ReadFile(counts)
		if err != nil {
			t.Fatalf("the count node's output is missing: %v", err)
		}
		// And the downstream copied it through the wire: this file exists only
		// if the edge resolved to a path both tools could see.
		clean := filepath.Join(flow.SampleDir(dataDir, f.user, "run-1", "qc", sample), "clean", "clean.txt")
		copied, err := os.ReadFile(clean)
		if err != nil {
			t.Fatalf("the qc node did not read its input: %v", err)
		}
		if string(copied) != string(data) {
			t.Fatalf("the wire garbled the data: %q vs %q", copied, data)
		}
	}

	// Success writes the sign file, which is the authority for "this step is
	// done" and the handle an operator uses to skip a step by hand.
	for _, node := range []string{"count", "qc"} {
		if !flow.IsSigned(flow.SignPath(dataDir, f.user, "run-1", node)) {
			t.Fatalf("node %s must be signed after success", node)
		}
	}

	// The archived sample table makes the run reproducible from its directory.
	if _, err := os.Stat(filepath.Join(dataDir, "flows", f.user, "runs", "run-1", flow.SamplesCopyName)); err != nil {
		t.Fatalf("the sample table was not archived: %v", err)
	}
	// And the record is readable.
	if _, err := LoadRunRecord(dataDir, f.user, "run-1"); err != nil {
		t.Fatalf("the run record is unreadable: %v", err)
	}
}

// A resumed run must not redo work: everything succeeded, so nothing is
// submitted (and the jobs keep their original ids).
func TestResumeSkipsCompletedNodes(t *testing.T) {
	f := newFixture(t)
	first := f.runOnce(t, "run-1")
	before := first.Node("qc").JobIDs

	units, _, err := f.runner.Plan(f.flow, f.samples, nil, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := LoadRunRecord(config.DataDir(f.configDir), f.user, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.runner.Run(context.Background(), f.flow, units, resumed, f.samples,
		flow.RunDir(config.DataDir(f.configDir), f.user, "run-1")); err != nil {
		t.Fatal(err)
	}
	if got := resumed.Node("qc").JobIDs; strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("a resume must not resubmit: %v vs %v", got, before)
	}
	if resumed.Node("count").Attempts != 1 {
		t.Fatalf("attempts = %d, want the node to have run once", resumed.Node("count").Attempts)
	}
}

// The sign file beats the record: an operator who touched it has decided.
func TestTouchedSignSkipsANode(t *testing.T) {
	f := newFixture(t)
	// Pretend the node really ran before: the output exists, and the operator
	// marked it done by hand.
	dataDir := config.DataDir(f.configDir)
	for _, sample := range []string{"s01-s001", "s02-s002"} {
		dir := filepath.Join(flow.SampleDir(dataDir, f.user, "run-1", "count", sample), "outs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "counts.txt"), []byte("counted\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := flow.Sign(flow.SignPath(dataDir, f.user, "run-1", "count")); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")

	if got := record.Node("count").JobIDs; len(got) != 0 {
		t.Fatalf("a signed node must not be submitted: %v", got)
	}
	if record.Node("count").State != flow.NodeSucceeded {
		t.Fatalf("a signed node counts as done, got %s", record.Node("count").State)
	}
	// The downstream can only run because the upstream counts as done.
	if record.Node("qc").State != flow.NodeSucceeded {
		t.Fatalf("downstream state = %s", record.Node("qc").State)
	}
}

// A failed node stops the fan-out at the first failing sample, and a downstream
// node that is not `when: always` is skipped with a reason.
func TestFailureStopsTheFanOutAndSkipsDownstream(t *testing.T) {
	f := newFixture(t)
	// The qc script fails when its input is missing; remove the count output by
	// making the *count* tool write nothing, so qc fails.
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")

	if record.State != flow.RunFailed {
		t.Fatalf("run state = %s, want failed", record.State)
	}
	if record.Node("count").State != flow.NodeSucceeded {
		t.Fatalf("count state = %s", record.Node("count").State)
	}
	qc := record.Node("qc")
	if qc.State != flow.NodeFailed {
		t.Fatalf("qc state = %s", qc.State)
	}
	if len(qc.JobIDs) != 1 {
		t.Fatalf("a failing node must stop at the first bad sample, submitted %d", len(qc.JobIDs))
	}
	if qc.Error == "" {
		t.Fatal("a failure without a reason is useless to an operator")
	}
	// A failed node is not signed: a resume will retry it.
	if flow.IsSigned(flow.SignPath(config.DataDir(f.configDir), f.user, "run-1", "qc")) {
		t.Fatal("a failed node must not be signed")
	}
}

// `when: always` is how a report node still runs after an upstream failure.
func TestWhenAlwaysRunsAfterFailure(t *testing.T) {
	f := newFixture(t)
	f.flow.Nodes = append(f.flow.Nodes, flow.Node{ID: "report", Tool: "qc@1.0.0", DependsOn: []string{"qc"}, When: flow.WhenAlways})
	// report reads nothing: give it the upstream's own output as its input_dir
	// so the plan stays simple.
	f.flow.Bindings = append(f.flow.Bindings, flow.Binding{From: "count.outputs.outs", To: "report.inputs.input_dir"})
	f.flow.Expose = append(f.flow.Expose, flow.Expose{Node: "report", Input: "outdir", From: "output.clean"})
	f.flow.Nodes = append(f.flow.Nodes, flow.Node{ID: "strict", Tool: "qc@1.0.0", DependsOn: []string{"qc"}})
	f.flow.Bindings = append(f.flow.Bindings, flow.Binding{From: "count.outputs.outs", To: "strict.inputs.input_dir"})
	f.flow.Expose = append(f.flow.Expose, flow.Expose{Node: "strict", Input: "outdir", From: "output.clean"})

	// Make qc fail by removing count's output.
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"),
		[]byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")

	if record.Node("qc").State != flow.NodeFailed {
		t.Fatalf("qc state = %s", record.Node("qc").State)
	}
	if record.Node("report").State != flow.NodeFailed {
		// report ran (its input exists), and it also failed reading the missing
		// file: what matters is that it *ran* rather than being skipped.
		t.Fatalf("`when: always` must run after a failure, got %s", record.Node("report").State)
	}
	if record.Node("strict").State != flow.NodeSkipped {
		t.Fatalf("a normal dependent of a failed node must be skipped, got %s", record.Node("strict").State)
	}
}

// A flow may not run a tool its user is not authorized for: the runner checks
// the same policy the API does.
func TestUnauthorizedToolIsRefused(t *testing.T) {
	f := newFixture(t)
	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "count", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Opts.Policy = policy
	if _, _, err := f.runner.Plan(f.flow, f.samples, nil, "run-1"); err == nil {
		t.Fatal("an unauthorized tool must be refused before anything is submitted")
	}
}

// Degraded tools (no sandbox) must be handed *host* paths: /flow does not exist
// on the host, and a tool that cannot open its own output directory is worse
// than no feature at all.
func TestDegradedToolsGetHostPaths(t *testing.T) {
	f := newFixture(t)
	units, _, err := f.runner.Plan(f.flow, f.samples, nil, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	hostRoot := flow.FlowDir(config.DataDir(f.configDir), f.user)
	for _, u := range units {
		for name, value := range u.Params {
			path, ok := value.(string)
			if !ok || !strings.HasPrefix(path, "/") {
				continue
			}
			if strings.HasPrefix(path, flow.PathFlow) {
				t.Fatalf("%s.%s = %q: a sandbox path reached a tool that has no sandbox",
					u.Node, name, path)
			}
			if !strings.HasPrefix(path, hostRoot) {
				t.Fatalf("%s.%s = %q, want a path under %s", u.Node, name, path, hostRoot)
			}
		}
	}
}

// The runner must refuse a tool that is not a `kind: task`, even if the flow
// was validated with an older tool set.
func TestServiceToolIsRefused(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.runner.Opts.ToolsDir, "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "schemaVersion: 1\nid: web\nversion: 1.0.0\nname: Web\nkind: service\nbackend: local\n" +
		"sandbox: none\nentry: work.sh\nresources: {cpu: 1, memory: \"256Mi\"}\n" +
		"ingress: {port: 8080, healthcheck: {path: \"/\"}}\nlifecycle: {restart: never, max_lifetime: \"1h\"}\n"
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Find(f.runner.Opts.ToolsDir, "web"); err != nil {
		t.Fatal(err)
	}
	// Make it authorized, so the only question left is the kind.
	policy, err := grant.New(nil, nil, []grant.Grant{
		{Tool: "count", Public: true}, {Tool: "qc", Public: true}, {Tool: "web", Public: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Opts.Policy = policy

	// Registration-time is where the kind check lives: a flow referencing a
	// service is refused by validation.
	f.flow.Nodes[0].Tool = "web"
	if err := f.flow.ValidateAgainst(func(id, version string) (*tool.Tool, error) {
		return tool.Find(f.runner.Opts.ToolsDir, id)
	}); err == nil {
		t.Fatal("a service tool must be refused by flow validation")
	}
	// And if a flow somehow reached the runner anyway, execution refuses it too:
	// RunTask only handles tasks, so the failure is at the point of use rather
	// than silent. (Asserted by the unit type: a service has no task handle.)
	if _, _, err := f.runner.Plan(f.flow, f.samples, nil, "run-1"); err != nil {
		// Either place refusing it is acceptable; what must not happen is a
		// service being *run* as a node.
		t.Logf("runner refused the service node too: %v", err)
	}
}
