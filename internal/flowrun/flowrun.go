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
	"path/filepath"
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
	// Concurrency bounds how many jobs run at once, across the whole run
	// (0 → DefaultConcurrency). It is the flow's resource control: a login node
	// is shared, and "one flow" must not mean "all of it".
	Concurrency int
	// RetryBackoff is how long to wait before the nth retry of a unit
	// (0 → retryBackoff). Injected so tests do not sleep.
	RetryBackoff func(attempt int) time.Duration
	// Sleep waits; injected so tests can assert backoff without waiting for it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log receives progress lines (the CLI prints them; tests collect them).
	Log func(format string, a ...any)
}

// DefaultConcurrency is the flow-level parallelism cap.
//
// Four is a guess that errs low on purpose: the parallelism that matters in a
// bioinformatics flow is inside a tool (samples fanned out over `ata`), and
// SRCOS's own concurrency is there to keep a many-node flow from saturating a
// shared login node. Raise it with --concurrency when the host is big.
const DefaultConcurrency = 4

// retryBackoff is the wait before retry n (1-based): 30s, 2m, 5m, 10m, 20m…
//
// Growing, because a tool that failed is usually waiting for something external
// (a filesystem, a lock, a queue) and hammering it changes nothing.
func retryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 30 * time.Second
	case attempt == 2:
		return 2 * time.Minute
	case attempt == 3:
		return 5 * time.Minute
	case attempt == 4:
		return 10 * time.Minute
	default:
		return 20 * time.Minute
	}
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
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.RetryBackoff == nil {
		opts.RetryBackoff = retryBackoff
	}
	if opts.Sleep == nil {
		opts.Sleep = func(ctx context.Context, d time.Duration) error {
			if d <= 0 {
				return nil
			}
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
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

// unit is one job in flight (or waiting): a node, one sample row, and how many
// times it has been tried.
//
// Only the scheduling loop mutates a unit; the worker goroutines hand results
// back through a channel. That is what keeps a concurrent run free of locks and
// races: there is exactly one owner of the state.
type unit struct {
	plan     flow.Unit
	attempts int
	done     bool
	failed   bool
	err      string
	// jobID is the id of the job currently running for this unit (or the last
	// one it ran), which is what `flow cancel` needs.
	jobID string
	// running is true while the unit's job is in flight.
	running bool
	// retrying is true between a failure and the retry's launch: the node is
	// still making progress, so it must not be declared failed yet.
	retrying bool
	// notBefore delays a retry without holding a concurrency slot (the slot is
	// released when the job returns, not when the retry is decided).
	notBefore time.Time
}

func (u *unit) key() string { return u.plan.Node + "/" + u.plan.Segment }

// result is what a worker reports back.
type result struct {
	key        string
	jobID      string
	instanceID string
	err        error
}

// Run executes a plan, recording as it goes.
//
// `record` may be an existing (resumed) run: nodes already signed or marked
// succeeded are skipped, and units whose attempts are recorded keep their retry
// budget.
//
// Concurrency: up to Options.Concurrency jobs run at once, across the whole run.
// A node's sample units may run in parallel, and independent nodes may too —
// but a node never starts before its dependencies succeeded, and a node that
// failed stops launching new units (the ones already in flight are left to
// finish: killing a colleague's half-written output to save a few seconds is
// not a trade SRCOS makes on its own).
//
// Cancellation: Options/flow.CancelPath — a file, because `flow cancel` runs in
// another process. The loop notices it between jobs, stops launching, and marks
// the run cancelled; the in-flight jobs are stopped by whoever wrote the flag.
func (r *Runner) Run(ctx context.Context, f *flow.Flow, plan []flow.Unit, record *flow.Run, samples *flow.Samples, runDir string) error {
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
	record.Samples = len(plan)
	if record.State == "" || record.State == flow.RunRunning {
		record.State = flow.RunRunning
	}
	cancelPath := flow.CancelPath(r.Opts.DataDir, r.Opts.User, record.ID)
	if err := r.save(record); err != nil {
		return err
	}

	// Node status, seeded from the record so a resume is a continuation.
	status := map[string]string{}
	for i := range record.Nodes {
		status[record.Nodes[i].ID] = record.Nodes[i].State
	}

	units := make([]*unit, 0, len(plan))
	for _, u := range plan {
		units = append(units, &unit{plan: u})
	}

	// Node-level progress, derived from the units after every result.
	nodeOrder := flow.NodeIDs(plan)
	unitsOf := map[string][]*unit{}
	for _, u := range units {
		unitsOf[u.plan.Node] = append(unitsOf[u.plan.Node], u)
	}

	inFlight := 0
	results := make(chan result, len(units)+1)
	cancelled := false
	// A node with a permanently failed unit stops launching the rest of its
	// samples: the failure is known, and the remaining units would only add
	// noise (and load) to a run that is already over for that node.
	permanentFailure := map[string]bool{}

	prepareNode := func(nodeID string) *flow.NodeState {
		ns := record.Node(nodeID)
		if ns.State == "" {
			ns.State = flow.NodePending
		}
		return ns
	}

	for {
		if !cancelled && flow.IsCancelled(cancelPath) {
			cancelled = true
			r.Opts.Log("cancel requested: no new jobs will be submitted")
		}

		now := r.Opts.Now()
		launched := 0
		if !cancelled {
			for _, nodeID := range nodeOrder {
				if inFlight >= r.Opts.Concurrency {
					break // the window is the bound for the whole run, not per node
				}
				node := nodeOf(f, nodeID)
				ns := prepareNode(nodeID)

				// A node that is already done (signed, or recorded as succeeded)
				// is skipped whole: its units are not even considered.
				if ns.State == flow.NodeSucceeded {
					continue
				}
				if signed := flow.IsSigned(flow.SignPath(r.Opts.DataDir, r.Opts.User, record.ID, nodeID)); signed {
					ns.State = flow.NodeSucceeded
					status[nodeID] = flow.NodeSucceeded
					r.Opts.Log("skip  %-14s (already done, signed)", nodeID)
					if err := r.save(record); err != nil {
						return err
					}
					continue
				}
				// Dependencies.
				if !depsSatisfied(node, status) && !depsTerminal(node, status) {
					continue // still waiting for an upstream to finish
				}
				if !depsSatisfied(node, status) {
					if node != nil && node.WhenOr() == flow.WhenAlways {
						if ns.State == flow.NodePending {
							r.Opts.Log("run   %-14s (when=always, upstream did not all succeed)", nodeID)
						}
					} else {
						if ns.State != flow.NodeSkipped {
							ns.State = flow.NodeSkipped
							ns.Error = "an upstream node did not succeed"
							ns.EndedAt = now.UTC()
							status[nodeID] = flow.NodeSkipped
							r.Opts.Log("skip  %-14s (upstream failed)", nodeID)
							if err := r.save(record); err != nil {
								return err
							}
						}
						continue
					}
				}
				// A node that failed stops launching new units.
				if ns.State == flow.NodeFailed || permanentFailure[nodeID] {
					continue
				}
				// The node is admitted; fill the concurrency window with its
				// pending units.
				for _, u := range unitsOf[nodeID] {
					if inFlight >= r.Opts.Concurrency {
						break
					}
					// A unit in flight must never be launched twice: the window
					// is filled across loop iterations, and the results arrive
					// asynchronously. A unit waiting out its retry backoff is
					// skipped by its `notBefore`, not by a flag — otherwise it
					// would wait forever.
					if u.done || u.failed || u.running || u.notBefore.After(now) {
						continue
					}
					if ns.State != flow.NodeRunning {
						ns.State = flow.NodeRunning
						ns.Attempts++
						if ns.StartedAt.IsZero() {
							ns.StartedAt = now.UTC()
						}
						status[nodeID] = flow.NodeRunning
						if err := r.save(record); err != nil {
							return err
						}
					}
					// Quota: the flow must not be a way around the aggregate
					// ceiling a grant sets for this user and tool.
					if err := r.checkQuota(u.plan, units); err != nil {
						u.failed = true
						u.err = err.Error()
						r.Opts.Log("fail  %-14s sample %s: %v", u.plan.Node, u.plan.Segment, err)
						continue
					}
					// The job is written here, in the loop, before the
					// goroutine starts it: a `flow cancel` in another process
					// must be able to find a running job by its id, and the id
					// only exists once the job has been submitted.
					jobID, err := r.submit(ctx, u.plan)
					if err != nil {
						if jobID != "" {
							ns.JobIDs = append(ns.JobIDs, jobID)
						}
						u.failed = true
						u.err = err.Error()
						permanentFailure[u.plan.Node] = true
						r.Opts.Log("fail  %-14s sample %s: %v", u.plan.Node, u.plan.Segment, err)
						continue
					}
					ns.JobIDs = append(ns.JobIDs, jobID)
					// Persist the id now: a `flow cancel` in another process
					// finds running jobs by reading this file, so a job that is
					// not yet recorded is a job that cannot be stopped.
					if err := r.save(record); err != nil {
						return err
					}
					u.attempts++
					u.running = true
					u.retrying = false
					u.jobID = jobID
					ns.AddUnitAttempt(u.plan.Segment)
					inFlight++
					launched++
					go func(u *unit, jobID string) {
						err := r.runJob(ctx, u.plan, jobID)
						results <- result{key: u.key(), jobID: jobID, err: err,
							instanceID: runtime.InstanceID(r.Opts.User, u.plan.ToolID, jobID)}
					}(u, jobID)
				}
			}
		}

		if inFlight == 0 {
			if launched > 0 {
				continue
			}
			// Nothing is running, nothing could be launched: either the run is
			// over, or units are waiting out their retry backoff. Waiting for
			// the earliest of those is the difference between "retry" and
			// "silently gave up".
			wait := time.Duration(0)
			found := false
			for _, u := range units {
				if u.done || u.failed || !u.retrying {
					continue
				}
				d := u.notBefore.Sub(now)
				if !found || d < wait {
					wait, found = d, true
				}
			}
			if !found {
				break // nothing running, nothing to launch, nothing to retry
			}
			if err := r.Opts.Sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}

		res := <-results
		inFlight--
		u := findUnit(units, res.key)
		if u == nil {
			continue
		}
		u.running = false
		ns := prepareNode(u.plan.Node)
		// The job id was already recorded when the unit was launched: recording
		// it again here would double-count every job in the run's history.
		if res.err == nil {
			u.done = true
			r.Opts.Log("ok    %-14s sample %s → %s", u.plan.Node, u.plan.Segment, res.jobID)
		} else {
			// Retry budget: per unit, not per node. Re-running the whole node
			// would redo the samples that already succeeded — safe (the tools
			// are idempotent) but wasteful, and the retry is usually about one
			// sample's input.
			node := nodeOf(f, u.plan.Node)
			maxRetry := 0
			if node != nil {
				maxRetry = node.RetryMax()
			}
			if !cancelled && u.attempts <= maxRetry {
				delay := r.Opts.RetryBackoff(u.attempts)
				u.notBefore = r.Opts.Now().Add(delay)
				u.retrying = true
				r.Opts.Log("retry %-14s sample %s in %s (attempt %d/%d): %v",
					u.plan.Node, u.plan.Segment, delay, u.attempts, maxRetry, res.err)
			} else {
				u.failed = true
				u.err = res.err.Error()
				permanentFailure[u.plan.Node] = true
				r.Opts.Log("fail  %-14s sample %s: %v", u.plan.Node, u.plan.Segment, res.err)
				// The node is over: units waiting out a retry will never be
				// launched, so they must stop being "waiting" — otherwise the
				// scheduler would keep sleeping for a retry that cannot happen.
				for _, other := range unitsOf[u.plan.Node] {
					if other.retrying {
						other.retrying = false
						other.failed = true
						if other.err == "" {
							other.err = "not retried: the node already failed"
						}
					}
				}
			}
		}

		// Recompute the node's state from its units, then persist.
		if err := r.settleNode(f, record, ns, unitsOf[u.plan.Node], status, cancelled); err != nil {
			return err
		}
		if err := r.save(record); err != nil {
			return err
		}
	}

	// Drain whatever is still in flight (a cancel cannot stop a running job from
	// this loop's point of view; it was stopped by whoever asked).
	for inFlight > 0 {
		<-results
		inFlight--
	}

	// Settle every node one last time (a node whose units all finished earlier
	// may not have been re-evaluated).
	for _, nodeID := range nodeOrder {
		_ = r.settleNode(f, record, prepareNode(nodeID), unitsOf[nodeID], status, cancelled)
	}

	record.EndedAt = r.Opts.Now().UTC()
	switch {
	case cancelled:
		record.State = flow.RunCancelled
	default:
		record.State = flow.RunSucceeded
		for _, ns := range record.Nodes {
			if ns.State == flow.NodeFailed {
				record.State = flow.RunFailed
				break
			}
		}
	}
	return r.save(record)
}

// settleNode derives a node's state from its units and writes the sign file when
// the node is done.
func (r *Runner) settleNode(f *flow.Flow, record *flow.Run, ns *flow.NodeState, units []*unit, status map[string]string, cancelled bool) error {
	if ns.State == flow.NodeSucceeded || ns.State == flow.NodeSkipped {
		return nil
	}
	depsOK := depsSatisfied(nodeOf(f, ns.ID), status)

	allDone, anyFailed, busy, completed := true, false, false, 0
	for _, u := range units {
		if !u.done {
			allDone = false
		} else {
			completed++
		}
		if u.failed {
			anyFailed = true
		}
		// A unit that is running, or waiting for its backoff, is still making
		// progress: the node cannot be declared failed underneath it.
		if u.running || u.retrying {
			busy = true
		}
	}

	switch {
	case cancelled && !allDone:
		// A cancelled run did not fail: its remaining work simply never ran, and
		// calling that "failed" would make every cancellation look like an error.
		ns.State = flow.NodeSkipped
		if ns.Error == "" {
			ns.Error = "run cancelled"
		}
		ns.EndedAt = r.Opts.Now().UTC()
		status[ns.ID] = flow.NodeSkipped
	case allDone && !anyFailed:
		ns.State = flow.NodeSucceeded
		ns.EndedAt = r.Opts.Now().UTC()
		status[ns.ID] = flow.NodeSucceeded
		// The sign file is the durable "this step is done", and writing it on
		// success is what makes the file the authority rather than a second
		// opinion.
		if err := flow.Sign(flow.SignPath(r.Opts.DataDir, r.Opts.User, record.ID, ns.ID)); err != nil {
			r.Opts.Log("warning: could not write the sign file for %s: %v", ns.ID, err)
		}
	case anyFailed && !busy:
		ns.State = flow.NodeFailed
		ns.EndedAt = r.Opts.Now().UTC()
		for _, u := range units {
			if u.failed && u.err != "" {
				ns.Error = fmt.Sprintf("%s: %s", u.plan.Segment, u.err)
				break
			}
		}
		// A node that failed stops launching its remaining samples; saying how
		// many did not complete keeps the record honest about what is missing —
		// whether they were never attempted or had their retries dropped.
		if completed < len(units) {
			ns.Error = fmt.Sprintf("%s (%d of %d sample(s) did not complete)",
				ns.Error, len(units)-completed, len(units))
		}
		status[ns.ID] = flow.NodeFailed
	case !depsOK && !cancelled && !busy:
		ns.State = flow.NodeSkipped
		if ns.Error == "" {
			ns.Error = "an upstream node did not succeed"
		}
		ns.EndedAt = r.Opts.Now().UTC()
		status[ns.ID] = flow.NodeSkipped
	}
	return nil
}

// checkQuota enforces the user's aggregate ceiling for the tool this unit runs.
//
// The count includes this run's in-flight jobs, not only the recorded instances:
// the instances appear as the jobs start, but three of them starting at once
// would each see only the others *after* the fact — and a quota that can be
// exceeded by starting work in parallel is not a quota.
func (r *Runner) checkQuota(u flow.Unit, all []*unit) error {
	if r.Opts.Policy == nil || u.Tool == nil {
		return nil
	}
	q := r.Opts.Policy.QuotaFor(r.Opts.User, u.Tool.ID)
	if q.IsZero() {
		return nil
	}

	live, err := runtime.ListInstances(r.Opts.ConfigDir)
	if err != nil {
		return nil // a quota check that cannot count must not block the work
	}
	used := grant.Usage{}
	for _, inst := range live {
		if inst.User != r.Opts.User || inst.Tool != u.Tool.ID || inst.State.Terminal() {
			continue
		}
		used.Instances++
		used.CPU += inst.CPURequest()
		used.Memory += inst.MemoryRequestBytes()
	}
	// This run's own in-flight units count: their instance records may not have
	// been written yet, and a quota that can be exceeded by starting work in
	// parallel is not a quota.
	for _, other := range all {
		if other == nil || other.plan.Tool == nil || other.plan.Tool.ID != u.Tool.ID || !other.running {
			continue
		}
		used.Instances++
		used.CPU += other.plan.Tool.Resources.CPU
		if other.plan.Tool.Resources.Memory != "" {
			if b, err := tool.ParseMemory(other.plan.Tool.Resources.Memory); err == nil {
				used.Memory += b
			}
		}
	}

	want := grant.Usage{Instances: 1, CPU: u.Tool.Resources.CPU}
	if u.Tool.Resources.Memory != "" {
		if b, err := tool.ParseMemory(u.Tool.Resources.Memory); err == nil {
			want.Memory = b
		}
	}
	return grant.CheckQuota(q, used, want)
}

func findUnit(units []*unit, key string) *unit {
	for _, u := range units {
		if u.key() == key {
			return u
		}
	}
	return nil
}

// runJob executes an already-submitted job to completion.
//
// It is called from a worker goroutine and must not touch runner state.
func (r *Runner) runJob(ctx context.Context, u flow.Unit, jobID string) error {
	dir := filepath.Join(config.JobsDir(r.Opts.ConfigDir, r.Opts.User, u.ToolID), jobID)
	j, err := job.Load(dir)
	if err != nil {
		return err
	}
	inst, err := r.Opts.Runner.RunTask(ctx, u.Tool, &job.Loaded{
		ID: jobID, Dir: dir, Path: filepath.Join(dir, "job.json"), Job: j,
	})
	if err != nil {
		return err
	}
	if inst.State != runtime.StateSucceeded {
		if inst.Error != "" {
			return fmt.Errorf("%s", inst.Error)
		}
		return fmt.Errorf("state %s", inst.State)
	}
	return nil
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

// depsTerminal reports whether every dependency has reached a final state (so a
// `when: always` node may proceed).
func depsTerminal(node *flow.Node, status map[string]string) bool {
	if node == nil {
		return true
	}
	for _, dep := range node.DependsOn {
		switch status[dep] {
		case flow.NodeSucceeded, flow.NodeFailed, flow.NodeSkipped:
		default:
			return false
		}
	}
	return true
}

// submit writes one job into the user's drop-box.
//
// Submitting and running are separate so the scheduler knows each job's id while
// it is still running — which is what makes `flow cancel` able to stop it.
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
	// The drop-box directory is the queue, and the flow runner is a consumer of
	// it exactly like a human running `srcos job run`: one code path, one set of
	// guarantees (sandbox, limits, instance record, audit).
	jobID, dir, err := job.Submit(r.Opts.ConfigDir, r.Opts.User, unit.ToolID, j)
	if err != nil {
		return "", err
	}
	// Take the submission before the gateway's task queue sees it. A flow node is
	// a step in a DAG, not an independent queue item: if the queue ran it out of
	// order (or twice) the pipeline would break in a way nobody could audit.
	// Claiming is the same mechanism `job run` uses, so all three consumers
	// agree on ownership.
	if _, err := job.Claim(dir); err != nil {
		r.Opts.Log("warning: could not claim flow job %s: %v", jobID, err)
	}
	return jobID, nil
}

// StopRun stops the jobs a run still has in flight.
//
// It reads the run record *from disk*, because stopping is a cross-process
// operation: `flow cancel` runs in another process than the scheduler, and the
// scheduler's in-memory copy is not shared state to reach into. Reading the file
// also means whatever the scheduler has already persisted is what gets stopped,
// which is the honest boundary.
//
// It reuses the stop path the reaper uses (backend by reference, or the recorded
// pid when there is no user systemd), so a cancelled task ends exactly the way a
// reaped one does — and its record ends up just as truthful.
//
// Jobs that already reached a terminal state are left alone: "stopping" a
// finished job would rewrite its history.
func (r *Runner) StopRun(ctx context.Context, f *flow.Flow, runID string) (int, error) {
	if r.Opts.Runner == nil {
		return 0, fmt.Errorf("this runner has no executor, so it cannot stop anything")
	}
	record, err := LoadRunRecord(r.Opts.DataDir, r.Opts.User, runID)
	if err != nil {
		return 0, err
	}
	stopped := 0
	var problems []string
	for i := range record.Nodes {
		node := nodeOf(f, record.Nodes[i].ID)
		if node == nil {
			continue
		}
		toolID := node.Tool
		if at := strings.IndexByte(toolID, '@'); at >= 0 {
			toolID = toolID[:at]
		}
		t, err := tool.Find(r.Opts.ToolsDir, toolID)
		if err != nil {
			problems = append(problems, fmt.Sprintf("node %s: %v", node.ID, err))
			continue
		}
		for _, jobID := range record.Nodes[i].JobIDs {
			inst, err := runtime.LoadInstance(runtime.InstancePath(
				r.Opts.ConfigDir, runtime.InstanceID(r.Opts.User, toolID, jobID)))
			if err != nil {
				continue // already gone
			}
			if inst.State.Terminal() {
				continue
			}
			if err := r.Opts.Runner.StopService(ctx, t, inst); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", inst.ID, err))
				continue
			}
			stopped++
		}
	}
	if len(problems) > 0 {
		return stopped, fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return stopped, nil
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

func nodeOf(f *flow.Flow, id string) *flow.Node {
	for i := range f.Nodes {
		if f.Nodes[i].ID == id {
			return &f.Nodes[i]
		}
	}
	return nil
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
