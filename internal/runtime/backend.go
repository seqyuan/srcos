// Package runtime executes a validated (Tool, Job) pair as a RunUnit.
//
// The shape is deliberately one primitive with two flavours (ADR-003):
//
//   - a *task* is a unit that runs to completion; its result is an exit code
//     and whatever it left in its declared outputs;
//   - a *service* is a unit that keeps running; its result is a loopback
//     endpoint the proxy can route to.
//
// Everything that differs between them is a policy applied to the same
// lifecycle: acquire -> materialize -> start -> (probe) -> wait/serve -> reap.
// Everything that decides *what the sandbox looks like* lives in package
// sandbox; everything that decides *where a unit runs* lives behind Backend.
package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// ExitStatus is how a unit finished.
type ExitStatus struct {
	Code int
	Err  error
}

// StartRequest is everything a backend needs to launch one unit.
//
// The backend never reads tool.yaml or job.json directly: it receives the
// already-resolved argv, mounts and environment. That keeps the SGE backend
// free of contract logic and makes both backends testable with the same
// fixtures.
type StartRequest struct {
	Tool *tool.Tool
	// Job is nil for a service.
	Job *job.Job
	// Spec is the materialized mount table.
	Spec *sandbox.Spec
	// Paths are the host paths.
	Paths Paths
	// View maps contract paths onto host paths for the current sandbox mode.
	View PathView
	// Argv is the command to execute inside the sandbox.
	Argv []string
	// Cwd is the working directory inside the sandbox.
	Cwd string
	// Env is the complete environment for the tool.
	Env []string
	// Limiter is the resource-control strategy the caller selected.
	Limiter Limiter
	// UnitName names the unit for backends that need a stable handle
	// (a systemd unit name, an SGE job name). Always derived from the instance
	// id, never from user input.
	UnitName string
	// LogPath receives stdout+stderr.
	LogPath string
	// InstanceID is used by backends that must publish an endpoint.
	InstanceID string
	// For a service, the backend must publish the endpoint it ends up on.
	// (Local binds inside the sandbox; SGE writes a rendezvous file.)
	WantEndpoint bool
}

// Handle is a started unit.
type Handle interface {
	// Ref identifies the unit inside its backend: a systemd unit name for
	// local, an SGE job id for sge. Recorded on the instance for audit and for
	// reconciliation after a restart.
	Ref() string
	// Wait blocks until the unit exits and returns its status. For a service
	// this returns only after Stop (or an unexpected crash).
	Wait(ctx context.Context) ExitStatus
	// Stop terminates the unit and waits for it to be gone. It must be
	// idempotent: the reaper and an explicit stop can race.
	Stop(ctx context.Context) error
	// Alive reports whether the unit is still running.
	Alive() bool
	// Endpoint returns the loopback endpoint for a service, once known.
	Endpoint() (route.Target, bool)
	// WantEndpoint reports whether this handle is authoritative about its own
	// endpoint. True for a backend that publishes one (SGE, over a rendezvous
	// file), false when the caller's port pool decided it (local).
	WantEndpoint() bool
	// Command is the rendered command line, for the instance record.
	Command() []string
	// Limiter names the resource-control mechanism actually used, so a
	// degraded run can be told apart from a properly limited one.
	Limiter() string
}

// PidReporter is implemented by handles that run a plain child process on this
// host, so the pid can be written into the instance record.
//
// It exists for the degraded path (no user systemd, ADR-014): there is no unit
// to stop by name, so a later SRCOS process — `svc stop`, the reaper, after a
// restart — has nothing but the recorded pid. A backend that owns its
// processes (a systemd unit, an SGE job) reports (0, 0) and is stopped through
// its own reference instead.
type PidReporter interface {
	// ChildPID returns the pid SRCOS started and its start time in clock ticks
	// (0 when unknown). The pair identifies one process incarnation; a bare pid
	// is a number the OS reuses.
	ChildPID() (pid int, start uint64)
}

// Backend launches units somewhere.
//
// Start returns as soon as the unit is launched, not when it finishes: that is
// what lets the same interface serve tasks (start, then Wait) and services
// (start, then serve traffic until Stop).
type Backend interface {
	// Name is "local" or "sge".
	Name() string
	// Start launches the unit. A non-nil error means the unit never started,
	// which is a SRCOS failure; a unit that starts and then fails is reported
	// through its ExitStatus instead.
	Start(ctx context.Context, req StartRequest) (Handle, error)
}

// Limiter is the resource-control strategy for one unit (ADR-014).
//
// Both spellings are carried because the two backends consume different ones:
// the local backend prefers systemd cgroup properties and falls back to
// RLIMITs, while the SGE backend translates the resources into qsub flags
// instead (SGE owns resource enforcement on a cluster).
type Limiter struct {
	Name string
	// SystemdProps are -p arguments for systemd-run.
	SystemdProps []string
	// PrlimitArgs are prlimit options (without the trailing "--").
	PrlimitArgs []string
}

// BuildLimiter renders the resource limits for a unit.
//
// Preferences, in order: systemd cgroup properties (real enforcement at the
// cgroup boundary, so MemoryMax OOM-kills rather than the process merely
// failing to allocate), then RLIMITs (inherited by children, which covers a
// work.sh that forks N sample processes, but RLIMIT_AS is a virtual-address
// ceiling and there is no RSS cap).
func BuildLimiter(res tool.Resources) Limiter {
	lim := Limiter{}
	if res.CPU > 0 {
		lim.SystemdProps = append(lim.SystemdProps, fmt.Sprintf("CPUQuota=%d%%", res.CPU*100))
	}
	if res.Memory != "" {
		if b, err := tool.ParseMemory(res.Memory); err == nil {
			lim.SystemdProps = append(lim.SystemdProps,
				"MemoryMax="+tool.FormatMemory(b),
				// Refuse to swap instead of OOM-killing: swapping a multi-GB
				// analysis turns a fast OOM into an unbounded hang.
				"MemorySwapMax=0")
			lim.PrlimitArgs = append(lim.PrlimitArgs, fmt.Sprintf("--as=%d", b))
		}
	}
	// Fork-bomb guard. TasksMax is per-cgroup, which is the semantics we want:
	// "this unit may have at most N tasks".
	lim.SystemdProps = append(lim.SystemdProps, "TasksMax=512")

	// Deliberately NOT setting RLIMIT_NPROC in the prlimit fallback.
	//
	// RLIMIT_NPROC counts processes for the *real UID across the whole
	// machine*, not per unit. On a busy login node that count is easily in the
	// thousands, so clamping it to 512 makes every clone() fail with EAGAIN —
	// including the ones bubblewrap needs to build its namespaces, which shows
	// up as the misleading "Creating new namespace failed: Resource
	// temporarily unavailable". A per-unit process cap is inherently a cgroup
	// feature; without a user systemd there is no correct way to express it,
	// so the fallback simply does not guard against fork bombs.
	if res.CPU > 0 {
		// RLIMIT_CPU is seconds of CPU time, not wall clock.
		lim.PrlimitArgs = append(lim.PrlimitArgs, fmt.Sprintf("--cpu=%d", res.CPU*3600))
	}
	return lim
}

// Options configures a Runner.
type Options struct {
	ConfigDir string
	ToolsDir  string
	User      string
	// Storages is the StorageProvider. Nil means "no shared data", which is
	// valid; a tool that requires a storage then fails loudly.
	Storages storage.Provider
	// Backends maps backend name to implementation. A tool whose backend is
	// absent fails with a clear message rather than silently using another one.
	Backends map[string]Backend
	// Routes is the dynamic routing table services publish into.
	Routes *route.Table
	// LogSink, when set, receives a live copy of the log.
	LogSink func(instanceID string, line []byte)
}

// Runner drives a unit through its lifecycle.
type Runner struct {
	opts  Options
	ports Ports
}

// NewRunner builds a Runner.
func NewRunner(opts Options) *Runner {
	if opts.Backends == nil {
		opts.Backends = map[string]Backend{}
	}
	return &Runner{opts: opts}
}

// Options exposes the configuration to callers that need to read it back
// (the CLI prints where things live).
func (r *Runner) Options() Options { return r.opts }

// ErrUnsupportedBackend is returned when a tool names a backend that is not
// registered on this host.
type ErrUnsupportedBackend struct {
	Tool    string
	Backend string
	Known   []string
}

func (e *ErrUnsupportedBackend) Error() string {
	return fmt.Sprintf("tool %s declares backend %q, which is not available here (registered: %v)",
		e.Tool, e.Backend, e.Known)
}

// backendFor picks the backend a tool declared.
//
// There is deliberately no fallback: a tool that says `backend: sge` must run
// on SGE. Silently running it locally would change its resource semantics and,
// for a qsubsge-style tool, could put heavy work on a login node.
func (r *Runner) backendFor(t *tool.Tool) (Backend, error) {
	if b, ok := r.opts.Backends[string(t.Backend)]; ok {
		return b, nil
	}
	known := make([]string, 0, len(r.opts.Backends))
	for name := range r.opts.Backends {
		known = append(known, name)
	}
	return nil, &ErrUnsupportedBackend{Tool: t.ID, Backend: string(t.Backend), Known: known}
}

// FirstLine trims a command's output to its first line, for error messages that
// must not paste a whole job banner into a log line.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
