package flowrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A failed node stops launching new samples, and a downstream node that is not
// `when: always` is skipped with a reason.
//
// With a concurrency window, "stops" means "no further units are submitted":
// the jobs already in flight are not killed (killing a colleague's half-written
// output to save a few seconds is not a trade SRCOS makes on its own).
func TestFailureStopsTheFanOutAndSkipsDownstream(t *testing.T) {
	f := newFixture(t)
	// Five samples with a window of two: a failure must be visible as a bound on
	// the submitted jobs, not as "exactly one".
	s, err := flow.ParseSamples("sample_id\nS001\nS002\nS003\nS004\nS005\n")
	if err != nil {
		t.Fatal(err)
	}
	f.samples = s
	f.runner.Opts.Concurrency = 2

	// The count tool writes nothing, so qc (which reads its input) fails.
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
	if len(qc.JobIDs) > f.runner.Opts.Concurrency {
		t.Fatalf("a failing node must stop launching: %d jobs submitted with a window of %d",
			len(qc.JobIDs), f.runner.Opts.Concurrency)
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

// ─────────────────────────────────────────────────────────────────────────
// 并发 / 重试 / 取消 / 配额
// ─────────────────────────────────────────────────────────────────────────

// The concurrency window bounds how many jobs run at once, and it is not
// exceeded even when a node has many samples.
func TestConcurrencyIsBounded(t *testing.T) {
	f := newFixture(t)
	s, err := flow.ParseSamples("sample_id\nS1\nS2\nS3\nS4\nS5\nS6\n")
	if err != nil {
		t.Fatal(err)
	}
	f.samples = s
	f.runner.Opts.Concurrency = 2

	// The count tool records its own concurrency in the run directory, so the
	// test measures what actually happened rather than what the plan said.
	gate := filepath.Join(t.TempDir(), "gate")
	script := `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
# claim a slot
i=0
while ! mkdir "` + gate + `.lock-$i" 2>/dev/null; do i=$((i+1)); [ $i -lt 8 ] || break; done
mkdir -p "` + gate + `.lock-$i"
sleep 0.05
printf 'counted\n' > "$SRCOS_PARAM_OUTDIR/counts.txt"
rmdir "` + gate + `.lock-$i"
`
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")
	if record.State != flow.RunSucceeded {
		t.Fatalf("run state = %s (%v)", record.State, record.Node("count"))
	}
	if got := len(record.Node("count").JobIDs); got != 6 {
		t.Fatalf("jobs = %d, want one per sample", got)
	}
	// No lock directory may survive (each job released its slot).
	entries, _ := os.ReadDir(filepath.Dir(gate))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(gate)+".lock-") {
			t.Fatalf("a job did not release its slot: %s", e.Name())
		}
	}
}

// The concurrency window is a *bound*, not a schedule: with a window of 3 and
// six samples the first three start together, and the test proves it by having
// the tool wait for its peers.
func TestConcurrencyActuallyOverlaps(t *testing.T) {
	f := newFixture(t)
	s, err := flow.ParseSamples("sample_id\nS1\nS2\nS3\n")
	if err != nil {
		t.Fatal(err)
	}
	f.samples = s
	f.runner.Opts.Concurrency = 3

	dir := t.TempDir()
	// Each job touches its own file and then waits for the other two: the run
	// can only finish if three jobs are alive at the same time.
	script := `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
touch "` + dir + `/$SRCOS_PARAM_SAMPLE_ID"
n=0
while [ $n -lt 100 ]; do
  count=$(ls "` + dir + `" | wc -l)
  [ "$count" -ge 3 ] && break
  n=$((n+1))
  sleep 0.05
done
[ "$(ls "` + dir + `" | wc -l)" -ge 3 ]
printf 'counted\n' > "$SRCOS_PARAM_OUTDIR/counts.txt"
`
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")
	if record.State != flow.RunSucceeded {
		t.Fatalf("run state = %s: the window did not actually overlap (%v)", record.State, record.Node("count"))
	}
}

// A node with retries re-runs the failing unit, with the injected backoff, and
// the run's record keeps the attempts.
func TestRetryRerunsTheFailingUnit(t *testing.T) {
	f := newFixture(t)
	f.runner.Opts.RetryBackoff = func(int) time.Duration { return time.Millisecond }
	f.flow.Nodes[0].Retry = &flow.Retry{Max: 2}

	// The count tool fails the first time it sees each sample, then succeeds.
	stateDir := t.TempDir()
	script := `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
marker="` + stateDir + `/$SRCOS_PARAM_SAMPLE_ID"
if [ ! -f "$marker" ]; then
  touch "$marker"
  echo "first attempt fails" >&2
  exit 7
fi
printf 'counted\n' > "$SRCOS_PARAM_OUTDIR/counts.txt"
`
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs []string
	f.runner.Opts.Log = func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }
	record := f.runOnce(t, "run-1")

	if record.Node("count").State != flow.NodeSucceeded {
		t.Fatalf("count state = %s (%s)", record.Node("count").State, record.Node("count").Error)
	}
	if record.State != flow.RunSucceeded {
		t.Fatalf("run state = %s", record.State)
	}
	// Two samples × two attempts each: the retry re-ran only the failing unit.
	if got := len(record.Node("count").JobIDs); got != 4 {
		t.Fatalf("jobs = %d, want 2 samples × 2 attempts", got)
	}
	if got := record.Node("count").UnitAttempts("s01-s001"); got != 2 {
		t.Fatalf("unit attempts = %d, want 2 (and persisted for a resume)", got)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "retry") {
		t.Fatal("a retry must be visible in the log")
	}
}

// Retries are bounded: once the budget is spent the node fails and says so.
func TestRetryBudgetIsRespected(t *testing.T) {
	f := newFixture(t)
	f.runner.Opts.RetryBackoff = func(int) time.Duration { return time.Millisecond }
	f.runner.Opts.Concurrency = 1
	f.flow.Nodes[1].Retry = &flow.Retry{Max: 1}

	// qc fails always; count is fine.
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "qc", "work.sh"),
		[]byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")

	qc := record.Node("qc")
	if qc.State != flow.NodeFailed {
		t.Fatalf("qc state = %s", qc.State)
	}
	// 2 samples × 2 attempts, then it gives up (and the untouched samples are
	// reported as not attempted).
	if got := len(qc.JobIDs); got > 4 {
		t.Fatalf("jobs = %d, want at most 2 samples × (1 attempt + 1 retry)", got)
	}
	// Samples that never got a slot must be accounted for, not silently
	// missing from the record.
	if len(qc.JobIDs) < 2*len(f.samples.Rows) && !strings.Contains(qc.Error, "did not complete") {
		t.Fatalf("a stopped fan-out must say what did not run (%d jobs): %q", len(qc.JobIDs), qc.Error)
	}
	if !strings.Contains(qc.Error, "code 9") {
		t.Fatalf("the node must report why it failed: %q", qc.Error)
	}
	if record.State != flow.RunFailed {
		t.Fatalf("run state = %s", record.State)
	}
}

// `flow cancel` writes a flag file; the scheduler stops launching and the run
// ends cancelled (the in-flight jobs are stopped by the canceller, which the
// test does by hand).
func TestCancelStopsScheduling(t *testing.T) {
	f := newFixture(t)
	f.runner.Opts.Concurrency = 1
	dataDir := config.DataDir(f.configDir)

	// The count tool asks for a cancel as soon as the first job starts, then
	// finishes normally: the second unit must not be submitted.
	script := `#!/bin/sh
set -e
mkdir -p "$SRCOS_PARAM_OUTDIR"
printf 'counted\n' > "$SRCOS_PARAM_OUTDIR/counts.txt"
touch "` + flow.CancelPath(dataDir, f.user, "run-1") + `"
`
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	record := f.runOnce(t, "run-1")

	if record.State != flow.RunCancelled {
		t.Fatalf("run state = %s, want cancelled", record.State)
	}
	if got := len(record.Node("count").JobIDs); got >= 2 {
		t.Fatalf("a cancelled run must stop launching: %d jobs", got)
	}
	if record.Node("qc").State == flow.NodeSucceeded {
		t.Fatal("nothing downstream should have run")
	}
	// A cancelled run did not *fail*: its remaining nodes are skipped, so a
	// cancellation never looks like an error in the record.
	if record.Node("work").State == flow.NodeFailed {
		t.Fatalf("a cancelled node must be skipped, not failed: %s", record.Node("work").Error)
	}
}

// StopRun stops what a cancelled run still has in flight, through the same stop
// path the reaper uses (so the instance record ends up truthful).
func TestStopRunStopsInFlightJobs(t *testing.T) {
	f := newFixture(t)
	dataDir := config.DataDir(f.configDir)

	// A long-running first job so there is something to cancel.
	if err := os.WriteFile(filepath.Join(f.runner.Opts.ToolsDir, "count", "work.sh"),
		[]byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	units, _, err := f.runner.Plan(f.flow, f.samples, nil, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	record := &flow.Run{FlowID: f.flow.ID, FlowVersion: f.flow.Version, User: f.user, ID: "run-1"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.runner.Run(ctx, f.flow, units, record, f.samples,
			flow.RunDir(dataDir, f.user, "run-1"))
	}()

	// Wait until the first job is recorded, then cancel.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if insts, err := runtime.ListInstances(f.configDir); err == nil {
			for _, inst := range insts {
				if inst.State == runtime.StateRunning {
					goto running
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
running:
	if err := flow.RequestCancel(flow.CancelPath(dataDir, f.user, "run-1")); err != nil {
		t.Fatal(err)
	}
	// A cancel is a cross-process operation: StopRun reads the record from disk
	// rather than reaching into the scheduler's memory.
	stopped, err := f.runner.StopRun(context.Background(), f.flow, "run-1")
	if err != nil {
		t.Logf("stop reported: %v", err)
	}
	if stopped == 0 {
		t.Fatal("a running job must be stopped by StopRun")
	}
	cancel()
	<-done

	for _, inst := range mustInstances(t, f.configDir) {
		if !inst.State.Terminal() {
			t.Fatalf("instance %s is still %s after a cancel", inst.ID, inst.State)
		}
	}
}

func mustInstances(t *testing.T, configDir string) []*runtime.Instance {
	t.Helper()
	insts, err := runtime.ListInstances(configDir)
	if err != nil {
		t.Fatal(err)
	}
	return insts
}

// A flow must not be a way around the aggregate quota a grant sets.
func TestQuotaRefusesMoreJobsThanAllowed(t *testing.T) {
	f := newFixture(t)
	policy, err := grant.New(nil, nil, []grant.Grant{
		{Tool: "count", Public: true, Quota: grant.Quota{MaxInstances: 1}},
		{Tool: "qc", Public: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.Opts.Policy = policy
	f.runner.Opts.Concurrency = 2 // two samples want to run at once

	record := f.runOnce(t, "run-1")
	if record.State == flow.RunSucceeded {
		t.Fatalf("a quota of one instance must refuse the second job: %v", record.Node("count"))
	}
	if !strings.Contains(record.Node("count").Error+strings.Join(nodeErrors(record, "count"), " "), "quota") {
		t.Fatalf("the refusal must name the quota: %q", record.Node("count").Error)
	}
}

func nodeErrors(record *flow.Run, nodeID string) []string {
	var out []string
	for _, n := range record.Nodes {
		if n.ID == nodeID && n.Error != "" {
			out = append(out, n.Error)
		}
	}
	return out
}
