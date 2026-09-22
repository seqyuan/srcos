// Package sge is the SGE/qsub backend.
//
// The shape of this backend follows ADR-015: **on a cluster an instance is a
// job, not a pod**. That single fact drives everything here.
//
//   - Resources are enforced by SGE, not by SRCOS. The tool's declaration is
//     translated into `-pe smp`, `-l h_vmem`, `-l h_rt`, `-q`; there is no
//     cgroup layer to add and adding one would fight the scheduler.
//   - The lifecycle ceiling is `h_rt`, not a SRCOS timer. Killing a job early
//     only to re-queue it is user-hostile, so idle reaping is opt-in here.
//   - There is no reliable inbound path from the scheduler to a login node, so
//     the *shared filesystem is the control channel* and an ssh local forward
//     is the data channel. The job writes what it became; the login node reads
//     it. No agent, no long-lived connection, no extra service.
//   - A suspended job is not a dead job. SGE preempts with SIGSTOP (`s` state)
//     and may requeue; conflating that with failure would make SRCOS re-submit
//     work that is merely paused.
//
// Commands are invoked through the Runner seam so the whole backend is
// testable without a cluster, which is the only reason this package can be
// developed before the login node is reachable.
package sge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// Runner executes external commands. Tests replace it; production uses Exec.
type Runner interface {
	// Run executes and returns combined stdout.
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the production Runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Config configures the backend.
type Config struct {
	// Binary paths. Empty means "look up on PATH".
	Qsub, Qstat, Qdel, Qalter string

	// SubmitDir is where job scripts are written, on the shared filesystem so
	// the compute node can read them.
	SubmitDir string
	// RendezvousDir is the shared control directory. It must be visible from
	// both the login node and the compute nodes.
	RendezvousDir string

	// SSH is the binary used for the local forward; empty means "ssh".
	SSH string
	// Tunnel enables the ssh -L data channel. When false the backend assumes
	// the gateway can dial the compute node directly (some clusters allow it),
	// which is faster and involves one less moving part.
	Tunnel bool
	// DirectDial, when true, skips the tunnel and publishes the compute node's
	// own address as the endpoint. Only valid if the login node can reach the
	// compute node.
	DirectDial bool

	// Scheduler holds cluster-wide defaults applied to every job.
	Scheduler SchedulerDefaults

	// PollEvery is how often the rendezvous and qstat are consulted.
	PollEvery time.Duration

	Runner Runner
}

// SchedulerDefaults are the site's cluster-wide qsub defaults. They live here
// rather than in tool.yaml because they describe the cluster, not the tool —
// the same reasoning as the 环境与数据 split.
type SchedulerDefaults struct {
	// DefaultQueue is used when a tool does not name one.
	DefaultQueue string
	// ThreadsPerSlot maps a tool's `cpu` onto `-pe <pe> <slots>`. HPC sites
	// differ on whether slots equal cores or threads.
	PE string
	// PEAccounting selects how a CPU request becomes a slot count:
	// "cores" (slots == cpu) or "threads" (slots == cpu * ThreadsPerCore).
	PEAccounting string
	// ThreadsPerCore is used only by the "threads" accounting mode.
	ThreadsPerCore int
	// Project is passed as `-P` when set (many sites gate fair-share on it).
	Project string
}

// rendezvousFor is the per-instance control directory.
func (c *Config) rendezvousFor(instanceID string) string {
	return filepath.Join(c.RendezvousDir, instanceID)
}

func (c *Config) pollEvery() time.Duration {
	if c.PollEvery > 0 {
		return c.PollEvery
	}
	return 2 * time.Second
}

func (c *Config) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c *Config) bin(name string) string {
	switch name {
	case "qsub":
		if c.Qsub != "" {
			return c.Qsub
		}
	case "qstat":
		if c.Qstat != "" {
			return c.Qstat
		}
	case "qdel":
		if c.Qdel != "" {
			return c.Qdel
		}
	case "qalter":
		if c.Qalter != "" {
			return c.Qalter
		}
	}
	return name
}

// Validate checks the configuration before any job is submitted.
func (c *Config) Validate() error {
	var problems []string
	if c.SubmitDir == "" {
		problems = append(problems, "SubmitDir is required (job scripts must land on a shared filesystem)")
	}
	if c.RendezvousDir == "" {
		problems = append(problems, "RendezvousDir is required (it is the only control channel)")
	}
	if c.Tunnel && c.DirectDial {
		problems = append(problems, "Tunnel and DirectDial are mutually exclusive")
	}
	if c.Scheduler.PE == "" {
		problems = append(problems, "Scheduler.PE is required (the parallel environment name, e.g. smp)")
	}
	switch c.Scheduler.PEAccounting {
	case "", "cores", "threads":
	default:
		problems = append(problems, "Scheduler.PEAccounting must be cores|threads")
	}
	if len(problems) > 0 {
		return fmt.Errorf("sge backend: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Backend implements runtime.Backend.
type Backend struct {
	Config Config
}

func (b *Backend) Name() string { return "sge" }

// Start writes a job script, submits it, and returns a handle.
//
// Submission itself is synchronous: qsub returns a job id or fails, and a
// failure here means the unit never existed (a SRCOS error), whereas a job that
// is submitted and then fails is reported through its exit code instead.
func (b *Backend) Start(ctx context.Context, req runtime.StartRequest) (runtime.Handle, error) {
	if err := b.Config.Validate(); err != nil {
		return nil, err
	}
	t, err := runtime.BuildInner(req.Tool, req.View, req.Spec, req.Cwd, req.Argv, req.Env)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(b.Config.SubmitDir, 0o755); err != nil {
		return nil, err
	}
	rd := b.Config.rendezvousFor(req.InstanceID)
	if err := os.MkdirAll(rd, 0o755); err != nil {
		return nil, err
	}
	// A stale endpoint from a previous run of the same instance would make the
	// healthcheck pass against a dead process, so the directory is cleared.
	if err := ClearRendezvous(rd); err != nil {
		return nil, err
	}

	script := b.renderScript(req, t)
	scriptPath := filepath.Join(b.Config.SubmitDir, req.UnitName+".sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return nil, err
	}

	args := b.qsubArgs(req)
	args = append(args, scriptPath)
	out, err := b.Config.runner().Run(ctx, b.Config.bin("qsub"), args...)
	if err != nil {
		return nil, fmt.Errorf("qsub %s: %w: %s", strings.Join(args, " "), err, runtime.FirstLine(out))
	}
	jobID, err := parseQsubOutput(out)
	if err != nil {
		return nil, fmt.Errorf("qsub said %q: %w", runtime.FirstLine(out), err)
	}
	if err := WriteRendezvous(rd, "jobid", jobID); err != nil {
		return nil, err
	}
	if err := WriteRendezvous(rd, "state", "submitted"); err != nil {
		return nil, err
	}

	return &jobHandle{
		cfg:       &b.Config,
		jobID:     jobID,
		instance:  req.InstanceID,
		unitName:  req.UnitName,
		isService: req.WantEndpoint,
		script:    scriptPath,
		command:   append([]string{b.Config.bin("qsub")}, args...),
	}, nil
}

// qsubArgs renders the resource request. This is where a tool's declaration
// becomes the scheduler's language; nothing downstream re-derives it.
func (b *Backend) qsubArgs(req runtime.StartRequest) []string {
	t := req.Tool
	res := t.Resources
	if req.Job != nil {
		res = job.EffectiveResources(req.Job, t)
	}
	sd := b.Config.Scheduler

	args := []string{
		"-N", req.UnitName,
		"-o", req.LogPath,
		"-e", req.LogPath,
		"-j", "y", // merge stderr into the same file: one log, one stream
		"-cwd",
		"-V", // forward the submission environment (the sandbox clears it anyway)
	}

	slots := res.CPU
	if slots < 1 {
		slots = 1
	}
	if sd.PEAccounting == "threads" && sd.ThreadsPerCore > 0 {
		slots *= sd.ThreadsPerCore
	}
	if sd.PE != "" {
		args = append(args, "-pe", sd.PE, strconv.Itoa(slots))
	}

	if res.Memory != "" {
		if b, err := tool.ParseMemory(res.Memory); err == nil {
			// h_vmem is a per-slot limit on SGE, so a tool asking for N cores
			// and M total needs M/N per slot. Getting this wrong either
			// under-requests (jobs OOM) or over-reserves (jobs never start).
			perSlot := b / uint64(slots)
			if perSlot == 0 {
				perSlot = b
			}
			args = append(args, "-l", "h_vmem="+tool.FormatMemory(perSlot))
		}
	}
	if res.Walltime != "" {
		args = append(args, "-l", "h_rt="+normalizeWalltime(res.Walltime))
	}
	if q := firstNonEmpty(res.Queue, sd.DefaultQueue); q != "" {
		args = append(args, "-q", q)
	}
	if sd.Project != "" {
		args = append(args, "-P", sd.Project)
	}
	if res.GPU > 0 {
		// GPU requests are site-specific (-l gpu=N is the common spelling, but
		// some clusters use a consumable or a specific PE). The generic form is
		// emitted so the site can override it with a patch later.
		args = append(args, "-l", fmt.Sprintf("gpu=%d", res.GPU))
	}
	return args
}

// normalizeWalltime converts H:MM:SS into SGE's HH:MM:SS.
func normalizeWalltime(w string) string {
	parts := strings.Split(strings.TrimSpace(w), ":")
	if len(parts) == 2 {
		return "0:" + parts[0] + ":" + parts[1]
	}
	return w
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ─────────────────────────────────────────────────────────────────────────
// Handle
// ─────────────────────────────────────────────────────────────────────────

type jobHandle struct {
	cfg       *Config
	jobID     string
	instance  string
	unitName  string
	isService bool
	script    string
	command   []string

	endpointOnce bool
	endpoint     route.Target
	endpointErr  error
}

func (h *jobHandle) Ref() string       { return h.jobID }
func (h *jobHandle) Command() []string { return h.command }
func (h *jobHandle) Limiter() string   { return "sge" }

// WantEndpoint is true: on a cluster the endpoint is decided by the compute
// node (and reached through a tunnel), not by the login node's port pool.
func (h *jobHandle) WantEndpoint() bool { return h.isService }

func (h *jobHandle) rd() string { return h.cfg.rendezvousFor(h.instance) }

// Endpoint blocks until the job has published the address it ended up on.
//
// This is the control channel in action: the job writes, the login node reads,
// and the shared filesystem is the only thing they share.
func (h *jobHandle) Endpoint() (route.Target, bool) {
	if h.endpointOnce {
		return h.endpoint, h.endpointErr == nil
	}
	h.endpointOnce = true

	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		ep, err := ReadEndpoint(h.rd())
		if err == nil {
			h.endpoint = ep
			return ep, true
		}
		h.endpointErr = err
		st, _ := ReadRendezvous(h.rd(), "state")
		if st == "exited" || st == "failed" {
			h.endpointErr = fmt.Errorf("job %s %s before publishing an endpoint", h.jobID, st)
			return route.Target{}, false
		}
		time.Sleep(h.cfg.pollEvery())
	}
	return route.Target{}, false
}

// Alive asks the scheduler, distinguishing "suspended" from "gone".
//
// A suspended job (SGE preemption, `s` state) is still the user's work; treating
// it as dead would make SRCOS tear down a service that is merely paused.
func (h *jobHandle) Alive() bool {
	st, err := h.queryState(context.Background())
	if err != nil {
		// The scheduler is unreachable. Fall back to the rendezvous, which the
		// job itself maintains — but never claim liveness on no evidence.
		rs, rerr := ReadRendezvous(h.rd(), "state")
		return rerr == nil && rs != "exited" && rs != "failed"
	}
	return st.Alive()
}

func (h *jobHandle) queryState(ctx context.Context) (JobState, error) {
	out, err := h.cfg.runner().Run(ctx, h.cfg.bin("qstat"), "-xml", "-j", h.jobID)
	if err != nil {
		// qstat exits non-zero when the job is unknown, which is the normal
		// way to learn that a job has finished and been forgotten.
		return JobState{Unknown: true}, nil
	}
	jobs, perr := ParseQstatXML(out)
	if perr != nil {
		return JobState{}, perr
	}
	for _, j := range jobs {
		if j.ID == h.jobID {
			return j.State, nil
		}
	}
	return JobState{Unknown: true}, nil
}

// Wait polls until the job is gone, then reports its exit status from the
// rendezvous file the job wrote.
//
// The exit code cannot come from qstat reliably: a finished job leaves the
// scheduler's view, and the accounting records may lag by minutes (or require
// `qacct`, which many sites restrict). The job writing its own exit code before
// it disappears is the only dependable channel.
func (h *jobHandle) Wait(ctx context.Context) runtime.ExitStatus {
	t := time.NewTicker(h.cfg.pollEvery())
	defer t.Stop()
	for {
		st, err := h.queryState(ctx)
		if err == nil && st.Unknown {
			return h.exitStatus()
		}
		if err == nil && st.Failed {
			return runtime.ExitStatus{Code: -1, Err: fmt.Errorf("job %s entered state %s", h.jobID, st.Raw)}
		}
		select {
		case <-ctx.Done():
			return runtime.ExitStatus{Code: -1, Err: ctx.Err()}
		case <-t.C:
		}
	}
}

func (h *jobHandle) exitStatus() runtime.ExitStatus {
	raw, err := ReadRendezvous(h.rd(), "exit_code")
	if err != nil {
		// The job vanished without writing one. That happens when SGE kills a
		// job for exceeding h_vmem or h_rt, so the error says so rather than
		// reporting a bare failure.
		return runtime.ExitStatus{Code: -1, Err: fmt.Errorf(
			"job %s disappeared without recording an exit code "+
				"(usual causes: exceeded h_vmem, exceeded h_rt, or qdel)", h.jobID)}
	}
	code, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return runtime.ExitStatus{Code: -1, Err: fmt.Errorf("job %s wrote an unparsable exit code %q", h.jobID, raw)}
	}
	return runtime.ExitStatus{Code: code}
}

// Stop deletes the job. Idempotent: deleting an already-gone job is success.
func (h *jobHandle) Stop(ctx context.Context) error {
	out, err := h.cfg.runner().Run(ctx, h.cfg.bin("qdel"), h.jobID)
	if err != nil {
		// "job not found" is the expected outcome of a race with completion.
		msg := strings.ToLower(out)
		if strings.Contains(msg, "not found") || strings.Contains(msg, "unknown job") ||
			strings.Contains(msg, "没有") {
			return nil
		}
		return fmt.Errorf("qdel %s: %w: %s", h.jobID, err, runtime.FirstLine(out))
	}
	_ = WriteRendezvous(h.rd(), "state", "deleted")
	return nil
}

// StopUnit implements runtime.UnitStopper: after a SRCOS restart only the
// persisted job id survives, and the reaper must still be able to stop it.
func (b *Backend) StopUnit(ctx context.Context, inst *runtime.Instance) error {
	ref := inst.BackendRef
	if ref == "" {
		return nil
	}
	out, err := b.Config.runner().Run(ctx, b.Config.bin("qdel"), ref)
	if err != nil {
		msg := strings.ToLower(out)
		if strings.Contains(msg, "not found") || strings.Contains(msg, "unknown job") {
			return nil
		}
		return fmt.Errorf("qdel %s: %w: %s", ref, err, runtime.FirstLine(out))
	}
	return nil
}

// UnitAlive implements runtime.UnitProber: the scheduler is the authority on
// whether a job exists, so a persisted job id is enough.
func (b *Backend) UnitAlive(ctx context.Context, inst *runtime.Instance) bool {
	ref := inst.BackendRef
	if ref == "" {
		return false
	}
	out, err := b.Config.runner().Run(ctx, b.Config.bin("qstat"), "-xml", "-j", ref)
	if err != nil {
		return false
	}
	jobs, perr := ParseQstatXML(out)
	if perr != nil {
		return false
	}
	for _, j := range jobs {
		if j.ID == ref {
			// Suspended still counts as alive: the job exists and holds its
			// slot, and SGE may resume it.
			return j.State.Alive()
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────
// 作业脚本
// ─────────────────────────────────────────────────────────────────────────

// renderScript builds the job script.
//
// The body is the same sandboxed argv the local backend would run, wrapped in
// the bookkeeping that makes the shared filesystem a usable control channel:
// publish the state, run, publish the exit code. Every write is atomic (write
// a temporary then rename) because the login node polls concurrently and a
// half-written file would be read as truth.
func (b *Backend) renderScript(req runtime.StartRequest, inner []string) string {
	rd := b.Config.rendezvousFor(req.InstanceID)
	isService := req.WantEndpoint

	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString("# Generated by SRCOS. Edits are overwritten on the next submission.\n")
	sb.WriteString("#$ -S /bin/bash\n")
	sb.WriteString("#$ -cwd\n\n")
	sb.WriteString("set -uo pipefail\n\n")
	fmt.Fprintf(&sb, "RD=%s\n", shellQuote(rd))
	sb.WriteString(`mkdir -p "$RD"
_pub() { printf '%s\n' "$2" > "$RD/$1.tmp" && mv "$RD/$1.tmp" "$RD/$1"; }
trap '_pub state failed; _pub exit_code 143' TERM INT
_pub state starting
_pub node "$(hostname)"
_pub job_id "${JOB_ID:-}"
_pub pid "$$"

`)
	if isService {
		sb.WriteString(b.serviceBody(req, inner))
	} else {
		sb.WriteString(b.taskBody(req, inner))
	}
	return sb.String()
}

func (b *Backend) taskBody(req runtime.StartRequest, inner []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", shellJoin(inner))
	sb.WriteString(`
_code=$?
_pub state exited
_pub exit_code "$_code"
exit "$_code"
`)
	return sb.String()
}

// serviceBody starts the unit in the background, waits for it to accept
// connections, publishes the address it ended up on, and then blocks.
//
// The port matters: two service jobs can land on the same compute node, so a
// fixed port would collide. The script therefore prefers the declared port and
// falls back to a free one, publishing whichever it got. The login node never
// guesses — it reads.
func (b *Backend) serviceBody(req runtime.StartRequest, inner []string) string {
	declared := 0
	if req.Tool.Ingress != nil {
		declared = req.Tool.Ingress.Port
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "DECLARED_PORT=%d\n", declared)
	sb.WriteString(`_free_port() {
  if command -v python3 >/dev/null 2>&1; then
    python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
    return
  fi
  echo "$DECLARED_PORT"
}
_port_in_use() {
  if command -v python3 >/dev/null 2>&1; then
    ! python3 - "$1" <<'PY'
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
  else
    return 1
  fi
}

PORT="$DECLARED_PORT"
if [ -z "$PORT" ] || [ "$PORT" = "0" ] || _port_in_use "$PORT"; then
  PORT="$(_free_port)"
fi
export SRCOS_PORT="$PORT"

`)
	// The sandbox argv already carries the environment, but SRCOS_PORT is only
	// known here, so the inner command is preceded by an explicit export for
	// the cases where the environment is inherited rather than set.
	fmt.Fprintf(&sb, "export SRCOS_PORT=%s\n", shellQuote("$PORT"))
	fmt.Fprintf(&sb, "%s &\n", shellJoin(inner))
	sb.WriteString(`CHILD=$!
_pub pid "$CHILD"

# Wait for the tool to accept connections before advertising the endpoint: a
# process that has started is not a service that is ready.
for _ in $(seq 1 240); do
  if ! kill -0 "$CHILD" 2>/dev/null; then
    _pub state exited
    _pub exit_code 1
    wait "$CHILD"
    exit $?
  fi
  if command -v python3 >/dev/null 2>&1; then
    if python3 - "$PORT" <<'PY'
import socket, sys
s = socket.socket()
s.settimeout(1)
sys.exit(0 if s.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
    then
      break
    fi
  else
    break
  fi
  sleep 0.5
done

_pub endpoint "$(hostname):$PORT"
_pub state running

wait "$CHILD"
_code=$?
_pub state exited
_pub exit_code "$_code"
exit "$_code"
`)
	return sb.String()
}

// ─────────────────────────────────────────────────────────────────────────
// 小工具
// ─────────────────────────────────────────────────────────────────────────

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellJoin renders argv as a shell command with every argument quoted, so a
// path containing a space or a quote cannot change the command it belongs to.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return "exec " + strings.Join(parts, " ")
}

// parseQsubOutput extracts the job id from qsub's stdout.
//
// Sites differ in what they print:
//
//	12345
//	Your job 12345 ("name") has been submitted
//	12345.1-1                                  (cell/task suffixed)
//	Your job-array 12345.1-10:1 has been submitted
//
// A bare "first integer on the line" rule is wrong: a warning such as
// "syntax error near line 3" would yield job 3. The number is therefore
// accepted only when the line is exactly a job id, or when it directly follows
// the word "job".
func parseQsubOutput(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if id, ok := jobIDToken(line); ok {
			return id, nil
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if strings.EqualFold(strings.Trim(f, "(),:\"'"), "job") && i+1 < len(fields) {
				if id, ok := jobIDToken(fields[i+1]); ok {
					return id, nil
				}
			}
		}
	}
	return "", errors.New("no job id in qsub output")
}

// jobIDToken accepts a bare integer, optionally with a .task-range suffix.
func jobIDToken(tok string) (string, bool) {
	tok = strings.Trim(tok, "().,\"'")
	if tok == "" {
		return "", false
	}
	head, _, _ := strings.Cut(tok, ".")
	if _, err := strconv.Atoi(head); err != nil {
		return "", false
	}
	return tok, true
}
