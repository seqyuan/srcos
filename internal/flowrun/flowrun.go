// Package flowrun executes a flow: it submits one ordinary SRCOS job per (node,
// sample row) in dependency order and records what happened.
//
// Two design points carry the weight:
//
//   - **A flow node is a normal task.** Same job.json, same sandbox, same quota
//     and audit trail as `srcos job run`. The flow is a *producer* of tasks, not
//     a second execution path — which is why a flow's jobs show up in the job
//     list, can be re-run by hand, and cannot bypass anything.
//   - **The run is resumable from its own directory.** The run record indexes
//     the jobs; a node's `.sign` file is the authority on "this step is done";
//     every path is derived from the run id, so a resumed run reuses the same
//     directories and the same inputs.
//
// Execution is sequential (one job at a time). That is deliberate for the first
// version: a login node shared with other work is not the place to discover
// that SRCOS's own parallelism interacts badly with a tool's. Sample-level
// parallelism inside one tool (ata / annotask) is unaffected — see ADR-005.
package flowrun

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// Options is the deployment the runner acts in.
type Options struct {
	ConfigDir string
	DataDir   string
	ToolsDir  string
	FlowsDir  string
	User      string
	// Runner executes one job. It is the same runner the CLI and the gateway
	// use: a flow adds ordering, not execution.
	Runner *runtime.Runner
	// Policy, when set, is checked per node: a flow must not run a tool its user
	// is not authorized to use.
	Policy *grant.Policy
	// Now is injectable so tests can pin run ids and timestamps.
	Now func() time.Time
	// Log receives progress lines (the CLI prints them; tests collect them).
	Log func(format string, a ...any)
}

// Runner executes flows.
type Runner struct{ Opts Options }

// New builds a runner.
func New(opts Options) *Runner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = func(string, ...any) {}
	}
	return &Runner{Opts: opts}
}

// Plan is a dry run: it resolves everything and submits nothing.
//
// It is the same code path as Run up to the point of execution, so what
// `--dry-run` prints is what a real run would do.
func (r *Runner) Plan(f *flow.Flow, samples *flow.Samples, params map[string]string, runID string) ([]flow.Unit, map[string]*tool.Tool, error) {
	manifests, err := r.resolveTools(f)
	if err != nil {
		return nil, nil, err
	}
	if runID == "" {
		runID = flow.NewRunID(f.ID, r.Opts.Now())
	}
	units, err := flow.Plan(flow.PlanInput{
		Flow: f, Samples: samples, Params: params, RunID: runID,
		Manifests: manifests, DataDir: r.Opts.DataDir, User: r.Opts.User,
		Form: r.pathForm(manifests),
	})
	if err != nil {
		return nil, nil, err
	}
	return units, manifests, nil
}

// pathForm renders a platform path the way one node's tool will see it.
//
// With a sandbox the flow's paths are literally `/flow/...` (the mount is part
// of the sandbox). Without one the tool sees the host filesystem, so the value
// must be the host path — the same substitution PathView makes for /workspace,
// and without it a degraded tool could not open anything the platform hands it.
func (r *Runner) pathForm(manifests map[string]*tool.Tool) func(nodeID, sandboxPath string) string {
	return func(nodeID, sandboxPath string) string {
		t := manifests[nodeID]
		if t == nil || t.Sandbox != tool.SandboxNone {
			return sandboxPath
		}
		root := flow.FlowDir(r.Opts.DataDir, r.Opts.User)
		return root + strings.TrimPrefix(sandboxPath, flow.PathFlow)
	}
}

// resolveTools loads every node's tool and checks the user may use it.
func (r *Runner) resolveTools(f *flow.Flow) (map[string]*tool.Tool, error) {
	out := map[string]*tool.Tool{}
	for _, n := range f.Nodes {
		id := n.Tool
		if i := strings.IndexByte(id, '@'); i >= 0 {
			id = id[:i]
		}
		t, err := tool.Find(r.Opts.ToolsDir, id)
		if err != nil {
			return nil, fmt.Errorf("node %q: tool %s: %w", n.ID, id, err)
		}
		if r.Opts.Policy != nil && !r.Opts.Policy.Allowed(r.Opts.User, t.ID) {
			return nil, fmt.Errorf("node %q: user %s is not authorized for tool %s", n.ID, r.Opts.User, t.ID)
		}
		out[n.ID] = t
	}
	return out, nil
}

// Run executes a plan: topologically, one job at a time, recording as it goes.
//
// `record` may be an existing (resumed) run: nodes already signed or marked
// succeeded are skipped, and their jobs are not submitted again.
func (r *Runner) Run(ctx context.Context, f *flow.Flow, units []flow.Unit, record *flow.Run, samples *flow.Samples, runDir string) error {
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return err
	}
	// Archive the sample table first: a run must be reproducible from its own
	// directory, and the operator's file will change tomorrow.
	if samples != nil && len(samples.Rows) > 0 {
		if err := archiveSamples(samples, runDir); err != nil {
			r.Opts.Log("warning: could not archive the sample table: %v", err)
		}
	}
	record.Samples = len(units)
	record.State = flow.RunRunning
	if err := r.save(record); err != nil {
		return err
	}

	nodeOrder := flow.NodeIDs(units)
	status := map[string]string{}
	for _, n := range record.Nodes {
		status[n.ID] = n.State
	}

	for _, nodeID := range nodeOrder {
		node := nodeOf(f, nodeID)
		ns := record.Node(nodeID)

		// Two authorities say "done": the sign file (which an operator may have
		// touched by hand) and the record.
		signed := flow.IsSigned(flow.SignPath(r.Opts.DataDir, r.Opts.User, record.ID, nodeID))
		if signed || ns.State == flow.NodeSucceeded {
			if ns.State != flow.NodeSucceeded {
				ns.State = flow.NodeSucceeded
			}
			r.Opts.Log("skip  %-14s (already done%s)", nodeID, signedNote(signed))
			status[nodeID] = flow.NodeSucceeded
			continue
		}

		// Dependencies: AND for a normal node, terminal-state for `when: always`.
		if !depsSatisfied(node, status) {
			if node != nil && node.WhenOr() == flow.WhenAlways {
				r.Opts.Log("run   %-14s (when=always, upstream did not all succeed)", nodeID)
			} else {
				ns.State = flow.NodeSkipped
				ns.Error = "an upstream node did not succeed"
				status[nodeID] = flow.NodeSkipped
				r.Opts.Log("skip  %-14s (upstream failed)", nodeID)
				if err := r.save(record); err != nil {
					return err
				}
				continue
			}
		}

		ns.State = flow.NodeRunning
		ns.Attempts++
		ns.StartedAt = r.Opts.Now().UTC()
		ns.Error = ""
		ns.JobIDs = nil
		status[nodeID] = flow.NodeRunning
		if err := r.save(record); err != nil {
			return err
		}

		r.Opts.Log("run   %-14s %d job(s)", nodeID, len(flow.UnitsForNode(units, nodeID)))
		failed := ""
		for _, unit := range flow.UnitsForNode(units, nodeID) {
			jobID, err := r.submit(ctx, unit)
			if jobID != "" {
				ns.JobIDs = append(ns.JobIDs, jobID)
			}
			if err != nil {
				failed = err.Error()
				r.Opts.Log("fail  %-14s sample %s: %v", nodeID, unit.Segment, err)
				break
			}
			r.Opts.Log("ok    %-14s sample %s → %s", nodeID, unit.Segment, jobID)
		}

		ns.EndedAt = r.Opts.Now().UTC()
		if failed != "" {
			ns.State = flow.NodeFailed
			ns.Error = failed
			status[nodeID] = flow.NodeFailed
		} else {
			ns.State = flow.NodeSucceeded
			status[nodeID] = flow.NodeSucceeded
			// The sign file is the durable "this step is done", and writing it
			// on success is what makes the file the authority rather than a
			// second opinion.
			if err := flow.Sign(flow.SignPath(r.Opts.DataDir, r.Opts.User, record.ID, nodeID)); err != nil {
				r.Opts.Log("warning: could not write the sign file for %s: %v", nodeID, err)
			}
		}
		if err := r.save(record); err != nil {
			return err
		}
	}

	record.EndedAt = r.Opts.Now().UTC()
	record.State = flow.RunSucceeded
	for _, ns := range record.Nodes {
		if ns.State == flow.NodeFailed {
			record.State = flow.RunFailed
			break
		}
	}
	return r.save(record)
}

// submit writes one job into the user's drop-box and runs it to completion.
func (r *Runner) submit(ctx context.Context, unit flow.Unit) (string, error) {
	// The unit's working directory exists before the tool runs: a tool may copy
	// or write into it without creating it first, and "mkdir -p" is not part of
	// the tool contract.
	if err := os.MkdirAll(flow.SampleDir(r.Opts.DataDir, r.Opts.User, unit.Tags["run"], unit.Node, unit.Segment), 0o755); err != nil {
		return "", err
	}
	j := &job.Job{
		SchemaVersion: 1,
		Name:          fmt.Sprintf("%s · %s", unit.Node, unit.Sample.ID),
		Params:        unit.Params,
		Outputs:       unit.Outputs,
		Tags:          unit.Tags,
	}
	if err := job.Validate(j, unit.Tool); err != nil {
		return "", err
	}
	jobID, dir, err := job.Submit(r.Opts.ConfigDir, r.Opts.User, unit.ToolID, j)
	if err != nil {
		return jobID, err
	}
	// The drop-box directory is the queue, and the flow runner is a consumer of
	// it exactly like a human running `srcos job run`: one code path, one set of
	// guarantees (sandbox, limits, instance record, audit).
	loaded := &job.Loaded{ID: jobID, Dir: dir, Path: dir + "/job.json", Job: j}
	inst, err := r.Opts.Runner.RunTask(ctx, unit.Tool, loaded)
	if err != nil {
		return jobID, err
	}
	if inst.State != runtime.StateSucceeded {
		msg := inst.Error
		if msg == "" {
			msg = fmt.Sprintf("state %s", inst.State)
		}
		return jobID, fmt.Errorf("%s", msg)
	}
	return jobID, nil
}

// archiveSamples copies the submitted table into the run directory.
func archiveSamples(samples *flow.Samples, runDir string) error {
	var b strings.Builder
	b.WriteString(strings.Join(samples.Columns, ","))
	b.WriteString("\n")
	for _, row := range samples.Rows {
		fields := make([]string, 0, len(samples.Columns))
		for _, c := range samples.Columns {
			value := row.Values[c]
			if strings.ContainsAny(value, ",\"\n") {
				value = `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
			}
			fields = append(fields, value)
		}
		b.WriteString(strings.Join(fields, ","))
		b.WriteString("\n")
	}
	return os.WriteFile(runDir+"/"+flow.SamplesCopyName, []byte(b.String()), 0o644)
}

// save writes the run record.
func (r *Runner) save(record *flow.Run) error {
	return flow.SaveRun(flow.RecordPath(r.Opts.DataDir, r.Opts.User, record.ID), record)
}

// depsSatisfied reports whether every dependency of a node succeeded.
func depsSatisfied(node *flow.Node, status map[string]string) bool {
	if node == nil {
		return true
	}
	for _, dep := range node.DependsOn {
		if status[dep] != flow.NodeSucceeded {
			return false
		}
	}
	return true
}

func nodeOf(f *flow.Flow, id string) *flow.Node {
	for i := range f.Nodes {
		if f.Nodes[i].ID == id {
			return &f.Nodes[i]
		}
	}
	return nil
}

func signedNote(signed bool) string {
	if signed {
		return ", signed"
	}
	return ""
}

// LoadRunRecord reads a run by id (for `flow resume` and `flow status`).
func LoadRunRecord(dataDir, user, runID string) (*flow.Run, error) {
	if !flow.ValidRunID(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	return flow.LoadRun(flow.RecordPath(dataDir, user, runID))
}

// ListRunRecords lists a user's runs, newest first (the run id sorts by time).
func ListRunRecords(dataDir, user string) ([]*flow.Run, error) {
	root := flow.RunsDir(dataDir, user)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*flow.Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, err := flow.LoadRun(flow.RecordPath(dataDir, user, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// DataDirFor derives the data directory from a config directory, so callers do
// not have to know the layout.
func DataDirFor(configDir string) string { return config.DataDir(configDir) }
