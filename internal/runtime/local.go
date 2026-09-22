package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// ─────────────────────────────────────────────────────────────────────────
// 两个 backend 共享的内层 argv
// ─────────────────────────────────────────────────────────────────────────

// BuildInner materializes the sandbox and returns the argv that must run
// *inside* whatever transport the backend uses.
//
// Local wraps it in a systemd scope/unit; SGE embeds it in a job script that
// runs on a compute node. Keeping this shared is what makes "the same tool
// definition runs on either backend" true rather than aspirational.
func BuildInner(t *tool.Tool, view PathView, spec *sandbox.Spec, cwd string, argv, env []string) ([]string, error) {
	switch t.Sandbox {
	case tool.SandboxBwrap, "":
		bwrapPath, ok, why := sandbox.BwrapProbe()
		if !ok {
			if strings.TrimSpace(why) == "" {
				// A probe that says "no" without saying why is a bug of its own;
				// failing with an empty message would hide it from whoever is
				// staring at the instance list.
				why = "bubblewrap is not usable on this host (the probe returned no reason)"
			}
			return nil, errors.New(why)
		}
		args := sandbox.BwrapArgv(spec, sandbox.BwrapOptions{Cwd: cwd, Env: env, Argv: argv})
		return append([]string{bwrapPath}, args...), nil

	case tool.SandboxNone:
		// Degraded mode. The environment is cleared by an inner `env -i`
		// rather than by replacing the outer command's Env: the resource
		// limiter needs the caller's DBUS_SESSION_BUS_ADDRESS and
		// XDG_RUNTIME_DIR to reach the user manager, so the wrapper must
		// inherit the host environment while the tool must not.
		return append(append([]string{"env", "-i"}, env...), argv...), nil

	case tool.SandboxApptainer:
		return nil, errors.New("sandbox: apptainer is not implemented yet (Phase 6)")

	default:
		return nil, fmt.Errorf("unsupported sandbox %q", t.Sandbox)
	}
}

// ResolveArgv decides what to execute for a unit.
//
// For a task, a work.sh inside the job directory means the submitter generated
// this run's script; otherwise the tool package's entry is used. A service has
// no job directory, so it always uses the tool entry.
func ResolveArgv(t *tool.Tool, view PathView, jobDir string) ([]string, error) {
	if jobDir != "" {
		if _, err := os.Stat(filepath.Join(jobDir, "work.sh")); err == nil {
			return []string{"bash", "work.sh"}, nil
		}
	}
	if t.Entry == "" {
		return nil, errors.New("no work.sh in the job directory and the tool declares no entry")
	}
	return []string{"bash", view.ToolDir(t) + "/" + t.Entry}, nil
}

// ─────────────────────────────────────────────────────────────────────────
// local backend
// ─────────────────────────────────────────────────────────────────────────

// Local runs units on this host.
//
// Tasks use a transient *scope* (`systemd-run --scope`), which blocks and
// propagates the exit code — the natural fit for run-to-completion work.
//
// Services use a transient *unit* (`systemd-run --unit=<name>`), which returns
// immediately and gives us what a long-running unit needs: cgroup limits,
// restart policy, a stable handle for `systemctl --user stop`, and a lifecycle
// systemd owns rather than SRCOS re-implementing.
//
// Both fall back to a plain child process when the user manager is absent,
// which is common on HPC login nodes (ADR-014).
type Local struct {
	// SystemdUser forces or forbids the user manager. Zero value auto-detects.
	SystemdUser *bool
	// Stdout receives a live copy of the log. Optional.
	Stdout func(instanceID string, line []byte)
}

func (l *Local) Name() string { return "local" }

// systemdAvailable probes `systemd-run --user --scope`. Cached: the probe
// costs a process spawn and the answer cannot change mid-run.
var systemdProbe struct {
	once sync.Once
	ok   bool
}

func (l *Local) useSystemd() bool {
	if l.SystemdUser != nil {
		return *l.SystemdUser
	}
	systemdProbe.once.Do(func() {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		systemdProbe.ok = exec.Command("systemd-run", "--user", "--scope", "--quiet", "--", "/bin/true").Run() == nil
	})
	return systemdProbe.ok
}

// Start launches the unit.
func (l *Local) Start(ctx context.Context, req StartRequest) (Handle, error) {
	inner, err := BuildInner(req.Tool, req.View, req.Spec, req.Cwd, req.Argv, req.Env)
	if err != nil {
		return nil, err
	}

	logFile, err := os.Create(req.LogPath)
	if err != nil {
		return nil, err
	}
	sink := sinkFor(logFile, req.InstanceID, l.Stdout)

	if req.Tool.Kind == tool.KindService {
		return l.startService(ctx, req, inner, logFile, sink)
	}
	return l.startTask(ctx, req, inner, logFile, sink)
}

// startTask runs a scope synchronously in a goroutine so Start can return a
// handle immediately, matching the Backend contract.
func (l *Local) startTask(ctx context.Context, req StartRequest, inner []string, logFile *os.File, sink io.Writer) (Handle, error) {
	path, args, limiterName := l.taskCommand(ctx, req, inner)

	cmd := exec.Command(path, args...)
	cmd.Stdout = sink
	cmd.Stderr = sink
	cmd.Stdin = nil
	if req.View.Degraded {
		cmd.Dir = req.Cwd
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	h := &processHandle{
		ref:     req.UnitName,
		cmd:     cmd,
		logFile: logFile,
		done:    make(chan ExitStatus, 1),
		limiter: limiterName,
		command: append([]string{path}, args...),
	}
	go func() {
		err := cmd.Wait()
		logFile.Close()
		h.done <- ExitStatus{Code: exitCode(err), Err: err}
	}()
	return h, nil
}

// taskCommand renders the task invocation, wrapping in the strongest resource
// limiter this host offers.
func (l *Local) taskCommand(ctx context.Context, req StartRequest, inner []string) (string, []string, string) {
	if l.useSystemd() {
		args := []string{"--user", "--scope", "--quiet"}
		for _, p := range req.Limiter.SystemdProps {
			args = append(args, "-p", p)
		}
		args = append(args, "--")
		return "systemd-run", append(args, inner...), "systemd-run"
	}
	if len(req.Limiter.PrlimitArgs) > 0 {
		if path, err := exec.LookPath("prlimit"); err == nil {
			args := append([]string{}, req.Limiter.PrlimitArgs...)
			return path, append(append(args, "--"), inner...), "prlimit"
		}
	}
	return inner[0], inner[1:], "none"
}

// startService launches a transient unit and returns as soon as it is
// registered, so the caller can healthcheck and publish a route.
func (l *Local) startService(ctx context.Context, req StartRequest, inner []string, logFile *os.File, sink io.Writer) (Handle, error) {
	h := &systemdUnitHandle{
		ref:     req.UnitName,
		unit:    req.UnitName,
		logFile: logFile,
		command: append([]string{"systemd-run"}, inner...),
		useUnit: l.useSystemd(),
	}
	if !h.useUnit {
		return l.startServiceFallback(req, inner, logFile, sink, h)
	}

	// A previous run of the same (user, tool) may have left a failed unit
	// behind, which would make this start fail on a name collision.
	_ = exec.Command("systemctl", "--user", "reset-failed", req.UnitName).Run()

	args := []string{"--user", "--unit", req.UnitName}
	for _, p := range req.Limiter.SystemdProps {
		args = append(args, "-p", p)
	}
	// A service is expected to be up for a long time; restart on failure gives
	// crash recovery without SRCOS polling for liveness.
	if req.Tool.Lifecycle != nil && req.Tool.Lifecycle.Restart != "" && req.Tool.Lifecycle.Restart != "never" {
		args = append(args, "-p", "Restart="+req.Tool.Lifecycle.Restart)
	}
	args = append(args, "--")
	cmd := exec.Command("systemd-run", append(args, inner...)...)
	cmd.Stdout = sink
	cmd.Stderr = sink
	h.command = append([]string{"systemd-run"}, append(args, inner...)...)

	if err := cmd.Run(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("systemd-run --unit %s: %w", req.UnitName, err)
	}
	return h, nil
}

// startServiceFallback runs a service as a plain child process. No cgroup
// limits and no restart policy apply; the caller is told which limiter was
// used so it can surface the degradation.
func (l *Local) startServiceFallback(req StartRequest, inner []string, logFile *os.File, sink io.Writer, h *systemdUnitHandle) (Handle, error) {
	cmd := exec.Command(inner[0], inner[1:]...)
	cmd.Stdout = sink
	cmd.Stderr = sink
	if req.View.Degraded {
		cmd.Dir = req.Cwd
	}
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	h.process = cmd
	h.command = inner
	go func() { _ = cmd.Wait(); logFile.Close() }()
	return h, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Handles
// ─────────────────────────────────────────────────────────────────────────

// fanoutWriter tees the instance log to an optional live sink so the CLI can
// stream it while the run is still in progress.
type fanoutWriter struct {
	log      io.Writer
	instance string
	extra    func(string, []byte)
}

func (w fanoutWriter) Write(p []byte) (int, error) {
	n, err := w.log.Write(p)
	if n > 0 && w.extra != nil {
		w.extra(w.instance, append([]byte(nil), p[:n]...))
	}
	return n, err
}

func sinkFor(logFile *os.File, instanceID string, extra func(string, []byte)) io.Writer {
	if extra == nil {
		return logFile
	}
	return fanoutWriter{log: logFile, instance: instanceID, extra: extra}
}

// processHandle is a task running as a direct child (systemd scope or plain
// process).
type processHandle struct {
	ref     string
	cmd     *exec.Cmd
	logFile *os.File
	done    chan ExitStatus
	limiter string
	command []string

	stopOnce sync.Once
}

func (h *processHandle) Ref() string { return h.ref }

// ChildPID reports the task process SRCOS started directly.
//
// A task needs this for the same reason a service does: without a user systemd
// the recorded pid is the only way to stop it later, and *with* one the scope
// systemd created for us has a generated name we cannot stop by reference — so
// the pid is the handle that actually works. `flow cancel` and the admin
// console's force-stop both depend on it.
func (h *processHandle) ChildPID() (int, uint64) {
	if h.cmd == nil || h.cmd.Process == nil {
		return 0, 0
	}
	pid := h.cmd.Process.Pid
	start, _ := pidStartTime(pid)
	return pid, start
}
func (h *processHandle) Command() []string              { return h.command }
func (h *processHandle) Limiter() string                { return h.limiter }
func (h *processHandle) Endpoint() (route.Target, bool) { return route.Target{}, false }
func (h *processHandle) WantEndpoint() bool             { return false }

func (h *processHandle) Wait(ctx context.Context) ExitStatus {
	select {
	case st := <-h.done:
		return st
	case <-ctx.Done():
		_ = h.Stop(context.Background())
		return ExitStatus{Code: -1, Err: ctx.Err()}
	}
}

func (h *processHandle) Stop(ctx context.Context) error {
	h.stopOnce.Do(func() {
		if h.cmd.Process != nil {
			// bwrap runs with --die-with-parent, so killing the direct child
			// takes the whole sandbox down with it.
			_ = h.cmd.Process.Signal(syscall.SIGTERM)
		}
	})
	select {
	case <-h.done:
		return nil
	case <-time.After(5 * time.Second):
	}
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("unit %s did not exit after SIGKILL", h.ref)
	}
	return nil
}

func (h *processHandle) Alive() bool {
	if h.cmd.Process == nil {
		return false
	}
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// systemdUnitHandle is a service running as a transient systemd unit (or, in
// the fallback, as a plain child process).
type systemdUnitHandle struct {
	ref     string
	unit    string
	useUnit bool
	process *exec.Cmd
	logFile *os.File
	command []string

	stopOnce sync.Once
}

func (h *systemdUnitHandle) Ref() string { return h.ref }

// ChildPID reports the process SRCOS started directly. A systemd unit reports
// (0, 0): systemd owns that child, and `systemctl stop` is its handle.
func (h *systemdUnitHandle) ChildPID() (int, uint64) {
	if h.useUnit || h.process == nil || h.process.Process == nil {
		return 0, 0
	}
	pid := h.process.Process.Pid
	start, _ := pidStartTime(pid)
	return pid, start
}

// Limiter names the mechanism holding the service's resources. "none" means the
// fallback ran it as a plain child process, i.e. no cgroup limits apply and the
// caller must surface that.
func (h *systemdUnitHandle) Limiter() string {
	if h.useUnit {
		return "systemd-run"
	}
	return "none"
}
func (h *systemdUnitHandle) Command() []string              { return h.command }
func (h *systemdUnitHandle) Endpoint() (route.Target, bool) { return route.Target{}, false }
func (h *systemdUnitHandle) WantEndpoint() bool             { return false }

func (h *systemdUnitHandle) Alive() bool {
	if !h.useUnit {
		if h.process == nil || h.process.Process == nil {
			return false
		}
		return processAlive(h.process.Process.Pid)
	}
	out, err := exec.Command("systemctl", "--user", "is-active", h.unit).Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func (h *systemdUnitHandle) Wait(ctx context.Context) ExitStatus {
	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		if !h.Alive() {
			return ExitStatus{Code: h.exitCode()}
		}
		select {
		case <-ctx.Done():
			return ExitStatus{Code: -1, Err: ctx.Err()}
		case <-t.C:
		}
	}
}

func (h *systemdUnitHandle) Stop(ctx context.Context) error {
	// Stopping is idempotent: the reaper and an explicit stop can race.
	h.stopOnce.Do(func() {
		if h.useUnit {
			_ = exec.Command("systemctl", "--user", "stop", h.unit).Run()
			return
		}
		if h.process != nil && h.process.Process != nil {
			_ = h.process.Process.Signal(syscall.SIGTERM)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !h.Alive() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if h.useUnit {
		_ = exec.Command("systemctl", "--user", "kill", "--signal=SIGKILL", h.unit).Run()
	} else if h.process != nil && h.process.Process != nil {
		_ = h.process.Process.Kill()
	}
	return nil
}

// exitCode reads the unit's exit status before the unit is reset.
func (h *systemdUnitHandle) exitCode() int {
	if !h.useUnit {
		return -1
	}
	out, err := exec.Command("systemctl", "--user", "show", "-p", "ExecMainStatus", "--value", h.unit).Output()
	if err != nil {
		return -1
	}
	var code int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &code); err != nil {
		return -1
	}
	return code
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// ─────────────────────────────────────────────────────────────────────────
// 按引用停止 / 探测
//
// These are the paths the reaper, `srcos svc stop` and startup reconciliation
// take after a SRCOS restart, when the only thing that survives is the
// persisted BackendRef. Without them a "stopped" service would keep serving
// traffic with no record saying so.
// ─────────────────────────────────────────────────────────────────────────

// StopUnit implements runtime.UnitStopper.
//
// It calls `stop` unconditionally rather than probing first:
// `systemctl list-units <pattern>` does not match a unit by bare name (it needs
// a glob), so a "does it exist?" pre-check silently reports false and the stop
// is then skipped — which is exactly how a service survives its own stop.
// Interpreting the stop's own error is both simpler and correct.
//
// Without a user systemd there is no unit to stop by name, so the recorded pid
// is used — that is the only handle a degraded instance has (ADR-014).
func (l *Local) StopUnit(ctx context.Context, inst *Instance) error {
	ref := inst.BackendRef
	if ref == "" && inst.PID == 0 {
		return nil
	}

	if l.useSystemd() && ref != "" {
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "stop", ref).CombinedOutput()
		if err == nil {
			// Reset so the unit name is free for the next start of the same
			// (user, tool).
			_ = exec.Command("systemctl", "--user", "reset-failed", ref).Run()
			return nil
		}
		msg := strings.ToLower(string(out))
		if !unitIsGone(msg) {
			return fmt.Errorf("systemctl --user stop %s: %w: %s", ref, err, strings.TrimSpace(string(out)))
		}
		// "Not loaded": either the unit exited on its own (a successful stop) or
		// it was started while the user manager was unavailable, in which case
		// the process is still there and the recorded pid is the way to end it.
	}

	if inst.PID == 0 {
		return fmt.Errorf("unit %s has no user systemd and no recorded pid, so SRCOS cannot stop it "+
			"(kill it manually if it is still running)", ref)
	}
	return stopRecordedProcess(ctx, inst.PID, inst.PIDStart)
}

// unitIsGone reports whether systemctl's complaint means the unit does not
// exist. Stopping races with a unit that exited on its own, and that is success.
func unitIsGone(message string) bool {
	for _, gone := range []string{"not loaded", "not found", "no such unit", "could not be found"} {
		if strings.Contains(message, gone) {
			return true
		}
	}
	return false
}

// UnitAlive implements runtime.UnitProber.
//
// A systemd unit is alive when it is "active" — activating, deactivating and
// failed all mean the service is not serving, which is what reconcile needs in
// order to choose between adopting and orphaning it. When there is no unit by
// that name, the recorded pid is the only signal, and it counts only if it
// still refers to the same process (otherwise the number was reused).
func (l *Local) UnitAlive(ctx context.Context, inst *Instance) bool {
	if l.useSystemd() && inst.BackendRef != "" {
		out, err := exec.CommandContext(ctx, "systemctl", "--user", "is-active", inst.BackendRef).Output()
		if err == nil && strings.TrimSpace(string(out)) == "active" {
			return true
		}
	}
	// No user manager, or no unit by that name: this is a service SRCOS started
	// as a plain child process.
	if inst.PID <= 0 || inst.PIDStart == 0 {
		return false
	}
	start, ok := pidStartTime(inst.PID)
	return ok && start == inst.PIDStart
}
