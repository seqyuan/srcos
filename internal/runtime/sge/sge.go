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
	"sync"
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
	// SSHUser is the login name for `ssh -L`; empty means the current user.
	SSHUser string
	// SSHArgs are extra ssh options (ProxyJump, a ControlMaster socket, ...).
	SSHArgs []string
	// Ports allocates the loopback port the tunnel listens on. Nil means a
	// service that needs a tunnel fails with a clear error instead of guessing.
	Ports PortAllocator
	// NewTunnel starts the data channel. Nil uses the real `ssh -L`; tests
	// replace it so the backend stays testable without a cluster.
	NewTunnel TunnelFactory
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

// PortAllocator hands out the loopback port a tunnel listens on. The runtime's
// shared port pool implements it, so a tunnel cannot collide with a local
// service's port.
type PortAllocator interface {
	Acquire(owner string) (int, error)
	Release(port int)
}

// TunnelRuntime is a started data channel (an `ssh -L` session in production).
type TunnelRuntime interface {
	Alive() bool
	Stop(ctx context.Context) error
}

// TunnelFactory starts the data channel for t. It exists so the backend can be
// tested without a cluster (and without spawning ssh).
type TunnelFactory func(ctx context.Context, t *Tunnel) (TunnelRuntime, error)

// defaultTunnelFactory runs the real `ssh -L`.
func defaultTunnelFactory(ctx context.Context, t *Tunnel) (TunnelRuntime, error) {
	if err := t.Start(ctx); err != nil {
		return nil, err
	}
	return t, nil
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
	// The caller hands us the login node's port-pool value in SRCOS_PORT, but on
	// a cluster the port is chosen by the job on the compute node and is only
	// known once the script runs. Replace it with a placeholder the rendered
	// script expands (see portPlaceholder), so a service is never told to bind
	// the login node's port.
	env := req.Env
	if req.WantEndpoint {
		env = withPortPlaceholder(env)
	}
	t, err := runtime.BuildInner(req.Tool, req.View, req.Spec, req.Cwd, req.Argv, env)
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

	tunnelMu   sync.Mutex
	tunnel     TunnelRuntime
	tunnelPort int
}

func (h *jobHandle) Ref() string       { return h.jobID }
func (h *jobHandle) Command() []string { return h.command }
func (h *jobHandle) Limiter() string   { return "sge" }

// WantEndpoint is true: on a cluster the endpoint is decided by the compute
// node (and reached through a tunnel), not by the login node's port pool.
func (h *jobHandle) WantEndpoint() bool { return h.isService }

func (h *jobHandle) rd() string { return h.cfg.rendezvousFor(h.instance) }

// Endpoint blocks until the job has published the address it ended up on, then
// makes it reachable from this host.
//
// This is the control channel in action: the job writes, the login node reads,
// and the shared filesystem is the only thing they share. On a cluster the
// published address names a compute node, so a tunnel turns it into a loopback
// port the gateway can proxy to.
func (h *jobHandle) Endpoint() (route.Target, bool) {
	if h.endpointOnce {
		return h.endpoint, h.endpointErr == nil
	}
	h.endpointOnce = true

	compute, err := h.waitComputeEndpoint()
	if err != nil {
		h.endpointErr = err
		return route.Target{}, false
	}
	if h.cfg.Tunnel {
		local, terr := h.startTunnel(compute)
		if terr != nil {
			h.endpointErr = terr
			return route.Target{}, false
		}
		h.endpoint = local
		return local, true
	}
	// No tunnel: publish the compute node's own address. The route layer only
	// accepts loopback, so this is reachable only when the gateway and the job
	// share a host (DirectDial).
	h.endpoint = compute
	return compute, true
}

// waitComputeEndpoint blocks until the job has published the address it ended
// up on.
func (h *jobHandle) waitComputeEndpoint() (route.Target, error) {
	deadline := time.Now().Add(5 * time.Minute)
	var last error
	for time.Now().Before(deadline) {
		ep, err := ReadEndpoint(h.rd())
		if err == nil {
			return ep, nil
		}
		last = err
		if st, _ := ReadRendezvous(h.rd(), FileState); terminalState(st) {
			return route.Target{}, fmt.Errorf("job %s %s before publishing an endpoint", h.jobID, st)
		}
		time.Sleep(h.cfg.pollEvery())
	}
	return route.Target{}, fmt.Errorf("job %s published no endpoint within 5m: %w", h.jobID, last)
}

// startTunnel brings the compute node's loopback port to a loopback port on
// this host. The tunnel is why "instances listen on loopback only" survives
// contact with a scheduler.
func (h *jobHandle) startTunnel(compute route.Target) (route.Target, error) {
	if h.cfg.Ports == nil {
		return route.Target{}, errors.New("sge service needs a port allocator for its ssh tunnel, but none is wired on this host")
	}
	port, err := h.cfg.Ports.Acquire(h.instance + "-tunnel")
	if err != nil {
		return route.Target{}, fmt.Errorf("allocate a tunnel port: %w", err)
	}
	factory := h.cfg.NewTunnel
	if factory == nil {
		factory = defaultTunnelFactory
	}
	tun := &Tunnel{
		LocalPort:  port,
		Node:       compute.Host,
		RemoteHost: "127.0.0.1",
		RemotePort: compute.Port,
		User:       h.cfg.SSHUser,
		SSHBin:     h.cfg.SSH,
		ExtraArgs:  h.cfg.SSHArgs,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	started, err := factory(ctx, tun)
	if err != nil {
		h.cfg.Ports.Release(port)
		return route.Target{}, fmt.Errorf("ssh -L to %s:%d: %w", compute.Host, compute.Port, err)
	}
	h.tunnelMu.Lock()
	h.tunnel = started
	h.tunnelPort = port
	h.tunnelMu.Unlock()
	return route.Target{Host: "127.0.0.1", Port: port}, nil
}

// stopTunnel tears down the data channel and returns its port to the pool.
// Idempotent: Stop can race the reaper.
func (h *jobHandle) stopTunnel(ctx context.Context) {
	h.tunnelMu.Lock()
	tun, port := h.tunnel, h.tunnelPort
	h.tunnel, h.tunnelPort = nil, 0
	h.tunnelMu.Unlock()
	if tun != nil {
		_ = tun.Stop(ctx)
	}
	if port != 0 && h.cfg.Ports != nil {
		h.cfg.Ports.Release(port)
	}
}

// Alive asks the scheduler, distinguishing "suspended" from "gone".
//
// A suspended job (SGE preemption, `s` state) is still the user's work; treating
// it as dead would make SRCOS tear down a service that is merely paused.
func (h *jobHandle) Alive() bool {
	// A running job whose data channel is gone is not a reachable service,
	// which is what Alive answers.
	h.tunnelMu.Lock()
	tun := h.tunnel
	h.tunnelMu.Unlock()
	if tun != nil && !tun.Alive() {
		return false
	}

	st, err := h.queryState(context.Background())
	if err != nil {
		// The scheduler is unreachable. Fall back to the rendezvous, which the
		// job itself maintains — but never claim liveness on no evidence.
		rs, rerr := ReadRendezvous(h.rd(), FileState)
		return rerr == nil && !terminalState(rs)
	}
	return st.Alive()
}

func (h *jobHandle) queryState(ctx context.Context) (JobState, error) {
	// `qstat -xml` (not `-xml -j <id>`): the `-j` form emits a
	// <detailed_job_info> document with no <job_list>/<state>, which this
	// parser does not read — it would always answer "unknown". Plain qstat is
	// scoped to the invoking user, which is exactly the set SRCOS owns.
	out, err := h.cfg.runner().Run(ctx, h.cfg.bin("qstat"), "-xml")
	if err != nil {
		// qstat exits non-zero when the scheduler is unreachable; the caller
		// decides (with confirmation) whether that means the job is gone.
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

// goneConfirmations is how many consecutive "the scheduler does not know this
// job" readings are required before Wait believes the job is gone.
//
// One is not enough: qstat exits non-zero both when a job is unknown and when
// the scheduler or the shared filesystem hiccups, so a single bad reading must
// not report a running job as finished.
const goneConfirmations = 3

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
	misses := 0
	for {
		st, err := h.queryState(ctx)
		if err == nil {
			switch {
			case st.Failed:
				return runtime.ExitStatus{Code: -1, Err: fmt.Errorf("job %s entered state %s", h.jobID, st.Raw)}
			case st.Unknown:
				// The job's own record is the stronger signal: a terminal state
				// written by the script settles it at once, while a bare qstat miss
				// must repeat before it is believed.
				if rs, rerr := ReadRendezvous(h.rd(), FileState); rerr == nil && terminalState(rs) {
					return h.exitStatus()
				}
				misses++
				if misses >= goneConfirmations {
					return h.exitStatus()
				}
			default:
				misses = 0
			}
		}
		select {
		case <-ctx.Done():
			return runtime.ExitStatus{Code: -1, Err: ctx.Err()}
		case <-t.C:
		}
	}
}

// terminalState reports whether the job script has recorded a state that no
// later transition follows.
func terminalState(s string) bool {
	return s == StateExited || s == StateFailed || s == StateDeleted
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
	defer h.stopTunnel(ctx)
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
	out, err := b.Config.runner().Run(ctx, b.Config.bin("qstat"), "-xml")
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
`)
	// The login node records the id qsub returned under the same name, so a
	// reader has one field to look at no matter which side wrote it last.
	fmt.Fprintf(&sb, "_pub %s \"${JOB_ID:-}\"\n", FileJobID)
	sb.WriteString(`_pub pid "$$"

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
  # No portable bind-and-release without python3: the declared port is the only
  # answer, and the caller fails loudly when there is none.
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
    # Approximate "in use" by "something already accepts connections there".
    (exec 3<>"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1
  fi
}
_ready() {
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$PORT" <<'PY'
import socket, sys
s = socket.socket()
s.settimeout(1)
sys.exit(0 if s.connect_ex(("127.0.0.1", int(sys.argv[1]))) == 0 else 1)
PY
  else
    (exec 3<>"/dev/tcp/127.0.0.1/$PORT") >/dev/null 2>&1
  fi
}

PORT="$DECLARED_PORT"
if [ -z "$PORT" ] || [ "$PORT" = "0" ] || _port_in_use "$PORT"; then
  PORT="$(_free_port)"
fi
if [ -z "$PORT" ] || [ "$PORT" = "0" ]; then
  # Without python3 SRCOS cannot pick a free port, and a tool must never be
  # handed an empty one. Fail here, where the reason is visible, instead of
  # advertising an endpoint nothing listens on.
  _pub state failed
  _pub exit_code 1
  echo "no usable port: install python3 on the compute node or set a free ingress.port" >&2
  exit 1
fi
export SRCOS_PORT="$PORT"

`)
	// SRCOS_PORT is only known here, so the inner command carries a placeholder
	// (withPortPlaceholder) that shellJoinPort expands to the port the job
	// actually got. It reaches both wrappers BuildInner can emit (`env -i` and
	// bwrap `--setenv`).
	fmt.Fprintf(&sb, "exec %s &\n", shellJoinPort(inner))
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
  if _ready; then
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

// portPlaceholder stands in for the runtime-chosen port until the job script
// expands it. The port is only known on the compute node, so it cannot be baked
// into the script text; the shell variable reference has to reach the tool
// unquoted, which shellQuote cannot express. A fixed token substituted after
// quoting is the way through both wrappers BuildInner emits.
const portPlaceholder = "__SRCOS_PORT__"

// withPortPlaceholder drops any SRCOS_PORT the caller supplied (it is the login
// node's port-pool value, meaningless on a compute node) and appends one whose
// value is the placeholder.
func withPortPlaceholder(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "SRCOS_PORT=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "SRCOS_PORT="+portPlaceholder)
}

// shellJoinPort is shellJoin for a service command, with the port placeholder
// replaced by an unquoted reference to the shell variable the job script sets.
// Substitution happens after quoting on purpose: the placeholder is present
// either as `SRCOS_PORT=<ph>` (env -i) or as a bare `<ph>` value following
// bwrap's `--setenv SRCOS_PORT`.
func shellJoinPort(inner []string) string {
	s := shellJoin(inner)
	s = strings.ReplaceAll(s, "'SRCOS_PORT="+portPlaceholder+"'", `SRCOS_PORT="$PORT"`)
	s = strings.ReplaceAll(s, "'"+portPlaceholder+"'", `"$PORT"`)
	return s
}

// shellJoin renders argv as a shell command with every argument quoted, so a
// path containing a space or a quote cannot change the command it belongs to.
//
// It deliberately does NOT prefix `exec`: the task body has to keep running
// after the command to publish the exit code, and `exec` would replace the
// script's shell and silently skip that bookkeeping. The service body, which
// wants the unit to replace a background subshell, adds `exec` itself.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
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
