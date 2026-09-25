// Package execute is the write path: the operations that make the platform do
// something — submit a run, cancel one, run a flow.
//
// It exists for the same reason package inspect does: two front-ends need the
// same answers (ADR-018). The REST API and the MCP server must not each decide
// what a valid submission is, when a quota is exceeded, or which tool a
// credential may drive; a second implementation would drift, and the drift
// would be a security bug here (one front-end checking the submit allowlist and
// the other not).
//
// The authorization model it enforces is two-dimensional (ADR-019 phase 2,
// roadmap §8 #9):
//
//   - **by user** — the owner's Grant policy decides which tools exist for
//     them, and their aggregate quota still applies. A token can never widen
//     this (ADR-019's subset rule).
//   - **by tool** — an agent token may carry a submit allowlist; a tool outside
//     it is refused even when the user is granted that tool.
//
// Validation, quotas and the job drop-box are therefore decided in exactly one
// place, and the front-ends only spell the answer (HTTP status / JSON-RPC).
package execute

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/flowrun"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// Error classes, mirroring inspect's: the front-ends map them onto their own
// vocabulary, and the classification is decided here so the two cannot
// disagree about what "not found" means.
var (
	// ErrNotFound: the thing does not exist, or the caller may not know that it
	// does (unknown and unauthorized stay one class, as on the read side).
	ErrNotFound = errors.New("not found")
	// ErrForbidden: the caller is known and this is refused (no submit scope, a
	// tool outside the allowlist, a quota exceeded, a tool not granted).
	ErrForbidden = errors.New("not permitted")
	// ErrBadRequest: the request is wrong (missing argument, a tool that is a
	// service, a job that fails the tool's own interface).
	ErrBadRequest = errors.New("invalid request")
	// ErrUnavailable: this deployment has no such subsystem (no tool directory,
	// no flow directory, no runner).
	ErrUnavailable = errors.New("not configured on this host")
)

// Grants is the authorization policy as the write path needs it: "may this user
// use this tool" and "how much may they take".
type Grants interface {
	Allowed(username, toolID string) bool
	QuotaFor(username, toolID string) grant.Quota
}

// Options wires a controller to one deployment.
//
// Every field is optional except the directories: a nil Grants means
// authorization is not wired (single-user deployment), and a nil Storages means
// tools declaring requires_storages fail loudly rather than silently.
type Options struct {
	// ConfigDir holds runtime state (data/instances, data/ws, the job drop-box).
	ConfigDir string
	// ToolsDir is the tool package root. Empty disables every operation.
	ToolsDir string
	// FlowsDir is the flow package root. Empty disables run_flow.
	FlowsDir string
	// Storages is the StorageProvider the runtime mounts.
	Storages storage.Provider
	// Grants is the policy. Nil means every tool is usable.
	Grants Grants
	// Supervisor stops instances: the gateway's own runner, which owns the
	// routing table and the port pool, so a cancel withdraws the route and
	// releases the port instead of leaving them to the next scan tick. Nil
	// falls back to a per-user runner (correct, just less tidy for services).
	Supervisor *runtime.Runner
	// NewRunner builds the runner that *starts* a unit for one user. Nil builds
	// the default local-backend runner.
	NewRunner func(user string) (*runtime.Runner, error)
	// Audit is the structured record of who did what (internal/audit). Nil is a
	// valid no-op, which is what a test or a single-user deployment wants.
	Audit *audit.Recorder
}

// Controller performs the write operations.
type Controller struct {
	opts   Options
	reader *inspect.Reader

	// mu serializes the quota check with the drop-box write. Without it a burst
	// of submissions each reads the same "nothing running yet" and all pass a
	// quota meant to admit one.
	mu sync.Mutex
	// wake tells the task queue that work is waiting. Set by NewQueue; nil in a
	// deployment that does not drain (the CLI, tests), where a submission simply
	// waits for whoever reads the drop-box.
	wake func()
}

// New builds a controller.
func New(opts Options) *Controller {
	return &Controller{
		opts: opts,
		reader: &inspect.Reader{
			ConfigDir: opts.ConfigDir,
			ToolsDir:  opts.ToolsDir,
			Storages:  opts.Storages,
			Grants:    grantsFor(opts.Grants),
		},
	}
}

// grantsFor narrows the policy to what the read side needs (it only asks
// "allowed"); keeping one object means the two cannot diverge.
func grantsFor(g Grants) inspect.Grants {
	if g == nil {
		return nil
	}
	return g
}

// ─────────────────────────────────────────────────────────────────────────
// submit
// ─────────────────────────────────────────────────────────────────────────

// SubmitRequest is one submission: the job.json fields plus addressing.
type SubmitRequest struct {
	Tool      string            `json:"tool"`
	Name      string            `json:"name"`
	Params    map[string]any    `json:"params"`
	Resources *tool.Resources   `json:"resources"`
	Outputs   []string          `json:"outputs"`
	Tags      map[string]string `json:"tags"`
}

// SubmitResult is what a caller gets back. The instance is queued at this
// point: the gateway's task loop starts it within moments (or at startup, if
// SRCOS was down when it was submitted).
type SubmitResult struct {
	JobID string `json:"jobId"`
	Dir   string `json:"dir"`
	Tool  string `json:"tool"`
	// InstanceID is the queued run's id, to poll with srcos_task_status / GET
	// /api/jobs/<id>/logs.
	InstanceID string `json:"instanceId,omitempty"`
	State      string `json:"state,omitempty"`
}

// Submit validates a job against its tool, checks the quota, and drops it into
// the queue — where the gateway's task loop picks it up and runs it.
//
// The queue is the same drop-box `srcos job submit` writes to (ADR-004: 目录即
// 队列), so a run submitted by an agent is an ordinary task with an ordinary
// record, log and artifacts.
func (c *Controller) Submit(ident agenttoken.Identity, req SubmitRequest) (res *SubmitResult, err error) {
	toolID := strings.TrimSpace(req.Tool)
	var t *tool.Tool
	defer func() {
		ev := audit.NewEvent(ident.AuditActor(), "submit").
			WithTarget("tool", toolID, versionOf(t)).
			WithParams(req.Params)
		if res != nil {
			ev = ev.WithRefs(map[string]string{"job": res.JobID, "instance": res.InstanceID})
		}
		c.recordAudit(ev, err)
	}()
	if toolID == "" {
		return nil, fmt.Errorf("%w: tool is required", ErrBadRequest)
	}
	if !ident.CanSubmitTool(toolID) {
		return nil, fmt.Errorf("%w: this credential may not submit to tool %s%s", ErrForbidden, toolID, allowlistHint(ident))
	}
	t, err = c.visibleTool(ident.User, toolID)
	if err != nil {
		return nil, err
	}
	if t.Kind != tool.KindTask {
		return nil, fmt.Errorf("%w: tool %s is a %s; services are started, not submitted", ErrBadRequest, t.ID, t.Kind)
	}

	j := &job.Job{
		SchemaVersion: 1,
		Name:          strings.TrimSpace(req.Name),
		Params:        req.Params,
		Tags:          req.Tags,
		Outputs:       req.Outputs,
		Resources:     req.Resources,
	}
	if j.Name == "" {
		j.Name = t.Name
	}
	if err := job.Validate(j, t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Quota after validation, so a request is first checked against what the
	// tool is willing to run with and then against what this user may consume.
	// It bounds *simultaneous* runs: a queued job does not count (see
	// State.ConsumesResources), so a user may queue more work than they can run
	// at once — which is what a queue is for — while the drain checks the same
	// ceiling again before each start.
	if err := c.checkQuota(ident.User, t, j); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}

	jobID, dir, err := job.Submit(c.opts.ConfigDir, ident.User, t.ID, j)
	if err != nil {
		return nil, err
	}
	loaded := &job.Loaded{ID: jobID, Dir: dir, Path: filepath.Join(dir, "job.json"), Job: j}
	instID := runtime.InstanceID(ident.User, t.ID, jobID)
	c.writePending(ident.User, t, loaded, instID)

	// Start now rather than at the next poll. The queue owns every start, so
	// this is a hint, not a second execution path: if the gateway is down, the
	// file waits and the first pass after startup runs it.
	if c.wake != nil {
		c.wake()
	}
	auditLine(ident, "submit", fmt.Sprintf("tool=%s@%s job=%s", t.ID, t.Version, jobID))
	return &SubmitResult{
		JobID:      jobID,
		Dir:        dir,
		Tool:       t.ID,
		InstanceID: instID,
		State:      string(runtime.StatePending),
	}, nil
}

// writePending records the queued instance before anything runs, so the id the
// caller is handed resolves immediately. Without it an agent's first status
// poll — which happens right after submit — would be told "no such instance".
//
// A `pending` record is also what tells the task queue this submission is work
// to start, as opposed to history (see Queue.nextJob).
func (c *Controller) writePending(user string, t *tool.Tool, loaded *job.Loaded, instanceID string) {
	paths := runtime.PathsFor(c.opts.ConfigDir, user, t.ID, loaded.ID)
	now := time.Now().UTC()
	inst := &runtime.Instance{
		ID:        instanceID,
		User:      user,
		Tool:      t.ID,
		Kind:      string(t.Kind),
		JobName:   loaded.Job.Name,
		State:     runtime.StatePending,
		Backend:   string(t.Backend),
		Sandbox:   string(t.Sandbox),
		LogPath:   paths.LogPath,
		WorkDir:   paths.JobDir,
		Outputs:   loaded.Job.Outputs,
		Tags:      loaded.Job.Tags,
		StartedAt: now,
	}
	path := runtime.InstancePath(c.opts.ConfigDir, instanceID)
	if err := runtime.SaveInstance(path, inst); err != nil {
		log.Printf("[srcos] submit %s: could not write the pending record: %v", instanceID, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// cancel
// ─────────────────────────────────────────────────────────────────────────

// CancelResult reports what was stopped.
type CancelResult struct {
	InstanceID string `json:"instanceId"`
	State      string `json:"state"`
}

// Cancel stops one of the caller's instances.
//
// It is idempotent on purpose: cancelling something that already finished is
// not an error, it is the caller's intent already satisfied — an agent that
// retries a cancel must not get a confusing failure.
func (c *Controller) Cancel(ctx context.Context, ident agenttoken.Identity, needle string) (res *CancelResult, err error) {
	needle = strings.TrimSpace(needle)
	var targetTool string
	defer func() {
		ev := audit.NewEvent(ident.AuditActor(), "cancel").WithTarget("tool", targetTool, "")
		if res != nil {
			ev = ev.WithRefs(map[string]string{"instance": res.InstanceID})
		}
		c.recordAudit(ev, err)
	}()
	if needle == "" {
		return nil, fmt.Errorf("%w: an instance id is required", ErrBadRequest)
	}
	// The lookup is user-scoped: another user's instance is "not found", never
	// "forbidden" (the read side's rule).
	view, others, err := c.reader.Instance(ident.User, needle)
	if err != nil {
		if len(others) > 0 {
			return nil, fmt.Errorf("%w: %v (candidates: %s)", ErrBadRequest, err, strings.Join(others, ", "))
		}
		return nil, translate(err)
	}
	targetTool = view.Tool
	if !ident.CanSubmitTool(view.Tool) {
		return nil, fmt.Errorf("%w: this credential may not control tool %s%s", ErrForbidden, view.Tool, allowlistHint(ident))
	}

	inst, err := runtime.LoadInstance(runtime.InstancePath(c.opts.ConfigDir, view.ID))
	if err != nil {
		return nil, fmt.Errorf("%w: instance %s", ErrNotFound, view.ID)
	}
	if inst.State.Terminal() {
		return &CancelResult{InstanceID: inst.ID, State: string(inst.State)}, nil
	}
	// The tool package is looked up *without* a grant check: cancelling your own
	// running instance must keep working after the tool is un-granted (the same
	// rule the read side applies to logs and artifacts).
	t, err := tool.Find(c.opts.ToolsDir, inst.Tool)
	if err != nil {
		return nil, fmt.Errorf("%w: tool package for %s is gone, cannot stop it", ErrUnavailable, inst.Tool)
	}
	runner, err := c.supervisor(ident.User)
	if err != nil {
		return nil, err
	}
	if err := runner.StopService(ctx, t, inst); err != nil {
		return nil, err
	}
	auditLine(ident, "cancel", fmt.Sprintf("instance=%s tool=%s", inst.ID, inst.Tool))
	return &CancelResult{InstanceID: inst.ID, State: string(inst.State)}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// run flow
// ─────────────────────────────────────────────────────────────────────────

// FlowRunRequest is one flow execution. Samples are the table's CSV text: an
// agent has no filesystem, and the table is small (it is a sample list, not
// data).
type FlowRunRequest struct {
	Flow        string            `json:"flow"`
	Samples     string            `json:"samples"`
	Params      map[string]string `json:"params"`
	Concurrency int               `json:"concurrency"`
}

// FlowRunResult is what the caller gets: the run id to watch with
// `srcos flow status` / the run record.
type FlowRunResult struct {
	RunID     string   `json:"runId"`
	Flow      string   `json:"flow"`
	Version   string   `json:"version"`
	Jobs      int      `json:"jobs"`
	Samples   int      `json:"samples"`
	Nodes     int      `json:"nodes"`
	State     string   `json:"state"`
	ToolsUsed []string `json:"toolsUsed,omitempty"`
}

// RunFlow expands a flow over a sample table and starts it in the background.
//
// The tool dimension of the submit authorization is applied to **every** tool
// the flow will use: a token narrowed to one pipeline must not be able to launder
// a different tool through a composed flow. The user dimension (grants, quotas)
// is enforced by the flow runner per node.
func (c *Controller) RunFlow(ident agenttoken.Identity, req FlowRunRequest) (res *FlowRunResult, err error) {
	defer func() {
		ev := audit.NewEvent(ident.AuditActor(), "run_flow").WithTarget("flow", req.Flow, "")
		if res != nil {
			ev = ev.WithTarget("flow", req.Flow, res.Version).
				WithRefs(map[string]string{"run": res.RunID})
		}
		c.recordAudit(ev, err)
	}()
	if strings.TrimSpace(c.opts.FlowsDir) == "" {
		return nil, fmt.Errorf("%w: no flow directory is configured", ErrUnavailable)
	}
	flowID := strings.TrimSpace(req.Flow)
	if flowID == "" {
		return nil, fmt.Errorf("%w: flow is required", ErrBadRequest)
	}
	if !flow.ValidFlowID(flowID) {
		return nil, fmt.Errorf("%w: invalid flow id %q", ErrBadRequest, flowID)
	}
	f, err := flow.Find(c.opts.FlowsDir, flowID)
	if err != nil {
		return nil, fmt.Errorf("%w: flow %s", ErrNotFound, flowID)
	}
	if err := f.ValidateAgainst(toolResolver(c.opts.ToolsDir)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	// Every tool the flow names must be within the credential's submit scope.
	for _, t := range flowTools(f) {
		if !ident.CanSubmitTool(t) {
			return nil, fmt.Errorf("%w: this credential may not run flow %s: it uses tool %s%s",
				ErrForbidden, f.ID, t, allowlistHint(ident))
		}
	}

	samples, err := flow.ParseSamples(req.Samples)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	runner, err := c.runnerFor(ident.User)
	if err != nil {
		return nil, err
	}
	ex := flowrun.New(flowrun.Options{
		ConfigDir:   c.opts.ConfigDir,
		DataDir:     config.DataDir(c.opts.ConfigDir),
		ToolsDir:    c.opts.ToolsDir,
		FlowsDir:    c.opts.FlowsDir,
		User:        ident.User,
		Runner:      runner,
		Policy:      policyFor(c.opts.Grants),
		Concurrency: req.Concurrency,
		Log:         func(format string, a ...any) { log.Printf("[srcos] flow "+format, a...) },
	})

	units, _, err := ex.Plan(f, samples, req.Params, "")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("%w: the sample table expands to no work", ErrBadRequest)
	}
	runID := units[0].Tags["run"]
	record := &flow.Run{
		FlowID:      f.ID,
		FlowVersion: f.Version,
		User:        ident.User,
		ID:          runID,
		Params:      req.Params,
		State:       flow.RunRunning,
		StartedAt:   time.Now().UTC(),
	}
	runDir := flow.RunDir(config.DataDir(c.opts.ConfigDir), ident.User, runID)
	// Persist the record before returning so the run is immediately visible to
	// `flow status` and to the resume path (Run re-saves it as it progresses).
	// The directory has to exist first: the record is written atomically through
	// a temp file beside it.
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return nil, err
	}
	recordPath := flow.RecordPath(config.DataDir(c.opts.ConfigDir), ident.User, runID)
	if err := flow.SaveRun(recordPath, record); err != nil {
		return nil, err
	}

	// The run outlives this call for the same reason a task does.
	go func() {
		if err := ex.Run(context.Background(), f, units, record, samples, runDir); err != nil {
			log.Printf("[srcos] flow run %s failed: %v", runID, err)
			return
		}
		log.Printf("[srcos] flow run %s finished: %s", runID, record.State)
	}()

	auditLine(ident, "run_flow", fmt.Sprintf("flow=%s@%s run=%s jobs=%d", f.ID, f.Version, runID, len(units)))
	return &FlowRunResult{
		RunID:     runID,
		Flow:      f.ID,
		Version:   f.Version,
		Jobs:      len(units),
		Samples:   len(samples.Rows),
		Nodes:     len(flow.NodeIDs(units)),
		State:     string(flow.RunRunning),
		ToolsUsed: flowTools(f),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// visibleTool resolves a tool the user is authorized to use, mapping the read
// side's errors onto this package's classes.
func (c *Controller) visibleTool(user, toolID string) (*tool.Tool, error) {
	if strings.TrimSpace(c.opts.ToolsDir) == "" {
		return nil, fmt.Errorf("%w: no tool directory is configured", ErrUnavailable)
	}
	t, err := c.reader.VisibleManifest(user, toolID)
	if err != nil {
		return nil, translate(err)
	}
	return t, nil
}

// translate maps an inspect error class onto the equivalent here, so the
// front-ends keep one mapping (inspect's) for both read and write.
func translate(err error) error {
	switch {
	case errors.Is(err, inspect.ErrBadRequest):
		return fmt.Errorf("%w: %v", ErrBadRequest, err)
	case errors.Is(err, inspect.ErrForbidden):
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	case errors.Is(err, inspect.ErrNotFound):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	case errors.Is(err, inspect.ErrUnavailable):
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		return err
	}
}

// checkQuota enforces the user's aggregate ceiling for a tool.
//
// The two ceilings are independent: the tool's declarations say "this is the
// most this analysis can use", the grant's quota says "this is the most you
// may take".
func (c *Controller) checkQuota(user string, t *tool.Tool, j *job.Job) error {
	if c.opts.Grants == nil {
		return nil
	}
	q := c.opts.Grants.QuotaFor(user, t.ID)
	if q.IsZero() {
		return nil
	}
	eff := job.EffectiveResources(j, t)
	want := grant.Usage{Instances: 1, CPU: eff.CPU}
	if eff.Memory != "" {
		if b, err := tool.ParseMemory(eff.Memory); err == nil {
			want.Memory = b
		}
	}
	used, err := c.usageFor(user, t.ID)
	if err != nil {
		return err
	}
	return grant.CheckQuota(q, used, want)
}

// usageFor sums what a user is *currently consuming* of one tool.
//
// Finished work does not count (a finished job holds nothing), and neither does
// queued work: `pending` is a queue position, and charging for it would make a
// queue impossible — the submission could not pass the check its own queue
// position implies. What the ceiling bounds is simultaneous consumption.
func (c *Controller) usageFor(user, toolID string) (grant.Usage, error) {
	all, err := runtime.ListInstances(c.opts.ConfigDir)
	if err != nil {
		return grant.Usage{}, err
	}
	var u grant.Usage
	for _, inst := range all {
		if inst.User != user || inst.Tool != toolID || !inst.State.ConsumesResources() {
			continue
		}
		u.Instances++
		u.CPU += inst.CPURequest()
		u.Memory += inst.MemoryRequestBytes()
	}
	return u, nil
}

// runnerFor builds the runner that starts a unit for one user. Tasks do not
// need the port pool or the routing table (the port pool's absence is even
// asserted by prepare), so a bare local runner is complete for them.
func (c *Controller) runnerFor(user string) (*runtime.Runner, error) {
	if c.opts.NewRunner != nil {
		return c.opts.NewRunner(user)
	}
	if strings.TrimSpace(c.opts.ToolsDir) == "" {
		return nil, fmt.Errorf("%w: no tool directory is configured", ErrUnavailable)
	}
	return runtime.NewRunner(runtime.Options{
		ConfigDir: c.opts.ConfigDir,
		ToolsDir:  c.opts.ToolsDir,
		User:      user,
		Storages:  c.opts.Storages,
		Audit:     c.opts.Audit,
		Backends:  map[string]runtime.Backend{"local": &runtime.Local{}},
	}), nil
}

// supervisor is the runner used to *stop* things: it must be the gateway's own,
// because it holds the routing table and the port pool a service release
// needs.
func (c *Controller) supervisor(user string) (*runtime.Runner, error) {
	if c.opts.Supervisor != nil {
		return c.opts.Supervisor, nil
	}
	return c.runnerFor(user)
}

// policyFor exposes the grant policy to the flow runner, which checks each node
// ("a flow must not run a tool its user is not authorized to use").
func policyFor(g Grants) *grant.Policy {
	if p, ok := g.(*grant.Policy); ok {
		return p
	}
	return nil
}

// toolResolver is the lookup flow validation needs: a tool by id, with an exact
// version match when the flow pinned one.
func toolResolver(toolsDir string) func(id, version string) (*tool.Tool, error) {
	return func(id, version string) (*tool.Tool, error) {
		t, err := tool.Find(toolsDir, id)
		if err != nil {
			return nil, fmt.Errorf("unknown tool %s", id)
		}
		if version != "" && t.Version != version {
			return nil, fmt.Errorf("tool %s is version %s here, but the flow asks for %s", id, t.Version, version)
		}
		return t, nil
	}
}

// flowTools lists the distinct tool ids a flow uses, version stripped. It is
// what the submit allowlist is checked against.
func flowTools(f *flow.Flow) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range f.Nodes {
		id := n.Tool
		if at := strings.IndexByte(id, '@'); at >= 0 {
			id = id[:at]
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// allowlistHint explains a refusal in the token's own terms, so an operator
// reading the error knows where to look.
func allowlistHint(ident agenttoken.Identity) string {
	if !ident.Agent {
		return ""
	}
	if len(ident.SubmitTools) == 0 {
		return " (its scopes: " + ident.Scopes.String() + ")"
	}
	return " (its submit allowlist: " + strings.Join(ident.SubmitTools, ",") + ")"
}

// auditLine writes the log line an auditor greps for: who, what, which tool and version,
// which instance. The REST/MCP layers already log every accepted agent request;
// this records the *act*, which is the fact that survives.
func auditLine(ident agenttoken.Identity, action, detail string) {
	log.Printf("[srcos] audit %s: %s %s", action, ident.Describe(), detail)
}

// recordAudit writes one write-path act to the structured audit stream. A nil
// recorder is a no-op, so a deployment without one (a test) needs no branch.
func (c *Controller) recordAudit(ev audit.Event, err error) {
	if err != nil {
		ev = ev.Denied(err.Error())
	} else {
		ev = ev.Allowed()
	}
	c.opts.Audit.Record(ev)
}

func versionOf(t *tool.Tool) string {
	if t == nil {
		return ""
	}
	return t.Version
}
