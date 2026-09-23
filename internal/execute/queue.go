package execute

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the consuming half of the drop-box: the daemon that actually
// runs what was submitted.
//
// It exists because "目录即队列" (ADR-004) was only half true until now: the
// directory *was* a queue, but the only consumer was a human typing `srcos job
// run`. An agent has no such human, and a submission that never runs is not an
// execution backend. With this loop, writing job.json is enough — which is what
// makes the submission path token-free and durable:
//
//   - When SRCOS is down, submissions pile up in the drop-box (they are files,
//     so nothing is lost). When it starts, the first pass picks them up.
//   - A submission wakes the loop, so the usual latency is milliseconds rather
//     than a tick.
//
// Two rules keep it honest:
//
//   - **One consumer per job.** The claim marker (job.Claim) means the gateway
//     and `srcos job run` in another process cannot both take the same job.
//   - **History is not retried.** A job whose instance record exists and is no
//     longer `pending` has been attempted; the daemon leaves it alone. A crash
//     mid-run shows up as a reconciled `stopped` record, not as an infinite
//     retry loop — `srcos job run --force` is the deliberate retry.
type Queue struct {
	ctrl *Controller

	// cap is the global in-flight limit. It is host protection, not per-user
	// fairness: a user's own ceiling is their grant quota. Per (user, tool) the
	// queue is serial regardless, because two runs of one tool in one workspace
	// would fight over the same outputs (and over the tool's own fanout).
	capv int
	// users lists the accounts to scan. Nil disables the queue entirely: a
	// deployment with no user registry has nobody to drain for.
	users func() []string
	log   func(format string, a ...any)

	mu       sync.Mutex
	inflight map[string]bool // "user/tool" → a run is in flight
	count    int
	noted    map[string]bool // dedupes "skipped because …" lines

	wake chan struct{}
}

// DefaultTaskWorkers is how many runs the queue keeps in flight across the
// whole host.
//
// Four is the same guess the flow runner makes, and for the same reason: the
// parallelism that matters is *inside* a tool (samples fanned out over `ata`),
// so SRCOS's own concurrency is only there to keep a burst from saturating a
// shared login node. The per-user truth is the grant quota.
const DefaultTaskWorkers = 4

// DefaultQueueInterval is the backstop poll. Submissions from the API wake the
// loop immediately; this interval covers work that appeared while SRCOS was
// down, or was written by the CLI in another process.
const DefaultQueueInterval = 5 * time.Second

// QueueOptions configures the consumer.
type QueueOptions struct {
	// Workers is the global in-flight limit. <=0 uses DefaultTaskWorkers.
	Workers int
	// Users lists the registered users, in any order. Nil disables the queue.
	Users func() []string
	// Log receives progress lines. Nil uses the standard logger.
	Log func(format string, a ...any)
}

// NewQueue builds the queue and connects it to the controller, so a submission
// starts immediately instead of at the next poll.
func NewQueue(ctrl *Controller, opts QueueOptions) *Queue {
	q := &Queue{
		ctrl:     ctrl,
		capv:     opts.Workers,
		users:    opts.Users,
		log:      opts.Log,
		inflight: map[string]bool{},
		noted:    map[string]bool{},
		wake:     make(chan struct{}, 1),
	}
	if q.capv <= 0 {
		q.capv = DefaultTaskWorkers
	}
	if q.log == nil {
		q.log = log.Printf
	}
	// The controller wakes the queue on submit. Setting it here (rather than
	// passing the queue into the controller) keeps the dependency one-way:
	// execute knows how to run work, the queue knows when to ask it to.
	ctrl.wake = q.Wake
	return q
}

// Wake asks the loop to drain now. It never blocks: a pending wake is as good
// as two, and a submission must not wait for the loop to notice.
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Run drains the queue until ctx ends. The first pass runs immediately, so a
// restart picks up whatever was submitted while SRCOS was down.
func (q *Queue) Run(ctx context.Context, interval time.Duration) {
	if q == nil || q.users == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultQueueInterval
	}
	if n := q.Tick(ctx); n > 0 {
		q.log("[srcos] task queue: started %d run(s) after startup", n)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-ticker.C:
		}
		q.Tick(ctx)
	}
}

// Tick scans every user's drop-boxes once and starts what it can.
//
// It returns how many runs it started, which is what tests and the startup log
// line need; the work itself continues in the background.
func (q *Queue) Tick(ctx context.Context) int {
	if q == nil || q.users == nil {
		return 0
	}
	started := 0
	for _, user := range q.users() {
		if ctx.Err() != nil {
			return started
		}
		for _, toolID := range q.workspaces(user) {
			if ctx.Err() != nil {
				return started
			}
			if !q.reserve(user, toolID) {
				continue // at the cap, or this (user, tool) is already running
			}
			loaded, t, ok := q.nextJob(user, toolID)
			if !ok {
				q.release(user, toolID)
				continue
			}
			taken, err := job.Claim(loaded.Dir)
			if err != nil {
				q.log("[srcos] task queue: claim %s: %v", loaded.Dir, err)
				q.release(user, toolID)
				continue
			}
			if !taken {
				// Another consumer (a `srcos job run` in another process) got
				// there first between our scan and our claim.
				q.release(user, toolID)
				continue
			}
			started++
			go q.run(user, t, loaded)
		}
	}
	return started
}

// workspaces lists the tools a user has a workspace for: <data>/ws/<user>/<tool>.
//
// It is deliberately a directory scan rather than `users × tools`: a tool
// directory exists only after the tool has been used (or a service started), so
// the scan visits exactly the drop-boxes that could hold work.
func (q *Queue) workspaces(user string) []string {
	root := filepath.Join(config.DataDir(q.ctrl.opts.ConfigDir), "ws", user)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// nextJob finds the oldest submission of one (user, tool) that has not been
// attempted, and reports the tool manifest to run it with.
func (q *Queue) nextJob(user, toolID string) (*job.Loaded, *tool.Tool, bool) {
	t, err := tool.Find(q.ctrl.opts.ToolsDir, toolID)
	if err != nil {
		q.noteOnce(user, toolID, "no tool package for %s (submissions under its workspace are left alone)", toolID)
		return nil, nil, false
	}
	if t.Kind != tool.KindTask {
		// A service's workspace can hold a submission only by mistake; services
		// are started with `srcos svc start`, not queued.
		return nil, nil, false
	}
	if !q.ctrl.granted(user, t.ID) {
		q.noteOnce(user, toolID, "user %s is not authorized for tool %s; its queued submissions are left alone", user, t.ID)
		return nil, nil, false
	}

	jobsDir := config.JobsDir(q.ctrl.opts.ConfigDir, user, toolID)
	scan, err := job.Scan(jobsDir)
	if err != nil {
		q.log("[srcos] task queue: scan %s: %v", jobsDir, err)
		return nil, nil, false
	}
	for _, broken := range scan.Broken {
		q.noteOnce(user, toolID, "ignoring unreadable submission: %s", broken)
	}

	// Oldest first: a queue that runs its newest submission first is not a
	// queue. File mtime is the submission time (the directory is created when
	// job.json is written).
	ordered := append([]*job.Loaded(nil), scan.Jobs...)
	sort.SliceStable(ordered, func(a, b int) bool { return submissionTime(ordered[a]).Before(submissionTime(ordered[b])) })

	for _, loaded := range ordered {
		if job.Claimed(loaded.Dir) {
			continue
		}
		// A flow node is not a queue item. The flow runner schedules its own
		// jobs in dependency order and has claimed them; treating one as an
		// independent submission would break the DAG (and its retry/resume
		// bookkeeping). The tag is written with the job, so this is decided from
		// the submission itself rather than from a race.
		if loaded.Job.Tags["run"] != "" {
			continue
		}
		state, recorded := q.attempted(user, t.ID, loaded.ID)
		switch {
		case !recorded, state == runtime.StatePending:
			// Never attempted, or queued by a submit that wrote the pending
			// record for addressability. Either way it is work to start.
		default:
			continue
		}
		if err := job.Validate(loaded.Job, t); err != nil {
			// A submission that cannot pass the tool's own contract will never
			// run; record why (visible in the UI) instead of retrying it
			// forever. The claim keeps it from being rescanned.
			if _, cerr := job.Claim(loaded.Dir); cerr == nil {
				q.recordFailure(user, t, loaded, fmt.Errorf("invalid submission: %w", err))
			}
			continue
		}
		// Quota is backpressure, not an error: leave the job queued and try
		// again next tick (the user's own runs will finish).
		if err := q.ctrl.checkQuota(user, t, loaded.Job); err != nil {
			return nil, nil, false
		}
		return loaded, t, true
	}
	return nil, nil, false
}

// submissionTime is the job's submission instant, falling back to the directory
// mtime when job.json cannot be stat'ed.
func submissionTime(loaded *job.Loaded) time.Time {
	if fi, err := os.Stat(loaded.Path); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(loaded.Dir); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// attempted reports whether an instance record exists for this submission, and
// in which state.
func (q *Queue) attempted(user, toolID, jobID string) (runtime.State, bool) {
	rec, err := runtime.LoadInstance(runtime.InstancePath(q.ctrl.opts.ConfigDir, runtime.InstanceID(user, toolID, jobID)))
	if err != nil {
		return "", false
	}
	return rec.State, true
}

// run executes one job and releases its slot.
//
// The context is deliberately not the tick's: a run outlives the pass that
// started it, and a shutdown mid-run must not mark the work cancelled — the
// process is owned by systemd, not by this loop, and a restart reconciles the
// record from what is actually still running.
func (q *Queue) run(user string, t *tool.Tool, loaded *job.Loaded) {
	defer q.release(user, t.ID)
	runner, err := q.ctrl.runnerFor(user)
	if err != nil {
		q.log("[srcos] task queue: %s/%s: %v", user, t.ID, err)
		q.recordFailure(user, t, loaded, err)
		return
	}
	inst, err := runner.RunTask(context.Background(), t, loaded)
	if err != nil {
		q.log("[srcos] task queue: %s/%s job %s: %v", user, t.ID, loaded.ID, err)
		q.recordFailure(user, t, loaded, err)
		return
	}
	line := fmt.Sprintf("[srcos] task queue: %s finished: %s", inst.ID, inst.State)
	if inst.Error != "" {
		line += " (" + inst.Error + ")"
	}
	q.log(line)
}

// recordFailure makes a failure that never reached a record visible, so it is
// neither invisible nor retried forever.
func (q *Queue) recordFailure(user string, t *tool.Tool, loaded *job.Loaded, cause error) {
	instID := runtime.InstanceID(user, t.ID, loaded.ID)
	path := runtime.InstancePath(q.ctrl.opts.ConfigDir, instID)
	if prev, err := runtime.LoadInstance(path); err == nil && prev.State != runtime.StatePending {
		return // the runner already recorded a real outcome
	}
	paths := runtime.PathsFor(q.ctrl.opts.ConfigDir, user, t.ID, loaded.ID)
	now := time.Now().UTC()
	inst := &runtime.Instance{
		ID:        instID,
		User:      user,
		Tool:      t.ID,
		Kind:      string(t.Kind),
		JobName:   loaded.Job.Name,
		State:     runtime.StateFailed,
		Error:     cause.Error(),
		Backend:   string(t.Backend),
		Sandbox:   string(t.Sandbox),
		LogPath:   paths.LogPath,
		WorkDir:   paths.JobDir,
		Outputs:   loaded.Job.Outputs,
		Tags:      loaded.Job.Tags,
		StartedAt: now,
		EndedAt:   now,
	}
	if err := runtime.SaveInstance(path, inst); err != nil {
		q.log("[srcos] task queue: could not record failure of %s: %v", instID, err)
	}
}

// ── slots ────────────────────────────────────────────────────────────────

// reserve takes a slot for one (user, tool). It refuses when the host is at its
// cap or that pair already has a run in flight: serial per pair is what keeps
// two runs of one tool out of each other's outputs.
func (q *Queue) reserve(user, toolID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := user + "/" + toolID
	if q.inflight[key] || q.count >= q.capv {
		return false
	}
	q.inflight[key] = true
	q.count++
	return true
}

func (q *Queue) release(user, toolID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := user + "/" + toolID
	if q.inflight[key] {
		delete(q.inflight, key)
		q.count--
	}
}

// noteOnce logs a per-(user, tool) explanation once per process run: a
// condition that will not change (a missing package, a revoked grant) must not
// fill the log every five seconds.
func (q *Queue) noteOnce(user, toolID, format string, a ...any) {
	key := user + "/" + toolID
	q.mu.Lock()
	seen := q.noted[key]
	q.noted[key] = true
	q.mu.Unlock()
	if !seen {
		q.log("[srcos] task queue: "+format, a...)
	}
}

// granted reports whether the user may use the tool (the user dimension; the
// credential dimension does not exist for work that no request asked for).
func (c *Controller) granted(user, toolID string) bool {
	if c.opts.Grants == nil {
		return true
	}
	return c.opts.Grants.Allowed(user, toolID)
}

// InFlight reports how many runs the queue currently has in flight (used by
// the admin surface and by tests).
func (q *Queue) InFlight() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.count
}
