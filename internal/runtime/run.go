package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/environment"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// Ports is the shared loopback port pool. A Runner needs one to start
// services; tasks do not use it.
type Ports interface {
	Acquire(owner string) (int, error)
	Release(port int)
	Reserve(port int, owner string) error
}

// ForUser returns a runner that starts units as `user` while sharing this
// runner's port pool, routing table, backends, storage provider and audit
// recorder (a shallow copy is enough: those fields are pointers or interfaces).
//
// It exists for the gateway, whose own runner deliberately carries no user
// (reconcile and the reaper act on records, not on behalf of an actor, and
// package runtime refuses to start a unit without one). An operator asking the
// console to start a service for a user needs a runner that does — but *not* a
// second port pool, which would hand the same port to two instances.
func (r *Runner) ForUser(user string) *Runner {
	if r == nil {
		return nil
	}
	clone := *r
	clone.opts.User = user
	return &clone
}

// SetPorts installs the port pool after construction (the pool is shared with
// the reconciler, which is built later).
func (r *Runner) SetPorts(p Ports) { r.ports = p }

// ports is nil until SetPorts; starting a service without it is a wiring bug,
// so it is reported clearly rather than panicking.
func (r *Runner) portPool() (Ports, error) {
	if r.ports == nil {
		return nil, errors.New("no port pool configured: service instances need one to be reachable")
	}
	return r.ports, nil
}

// ─────────────────────────────────────────────────────────────────────────
// 公共准备
// ─────────────────────────────────────────────────────────────────────────

// prepared is the resolved, validated shape both flavours need.
type prepared struct {
	tool     *tool.Tool
	paths    Paths
	view     PathView
	spec     *sandbox.Spec
	limiter  Limiter
	storages []storage.Storage
	// env is the unit's environment, without the port: a service appends
	// SRCOS_PORT once the pool has handed one over, and only then can a
	// declarative command be resolved.
	env []string
	// jobDir is a task's submission directory (a work.sh dropped there wins over
	// the tool's own declaration).
	jobDir string
	// jobCommand is a submission-time argv from job.json, which wins over
	// everything the tool declares.
	jobCommand []string
}

// argvFor resolves the unit's argv against its (now complete) environment.
//
// It is lazy because a declarative command may reference ${SRCOS_PORT}, which
// only exists after the port pool has answered.
func (p *prepared) argvFor(t *tool.Tool) ([]string, error) {
	if len(p.jobCommand) > 0 {
		return p.jobCommand, nil
	}
	return ResolveArgv(t, p.view, p.jobDir, p.env)
}

// jobCommandOf is job.json's explicit argv, if the submission carried one.
func jobCommandOf(j *job.Job) []string {
	if j == nil {
		return nil
	}
	return j.Command
}

// prepare resolves everything that does not depend on the backend: directories,
// templates, mount table, argv and resource limits.
//
// It is separate from start so that a failure here is reported before any
// port is taken or any unit is launched.
func (r *Runner) prepare(t *tool.Tool, j *job.Job, jobID string) (*prepared, error) {
	// A runner without a user can reconcile and reap records (they carry their
	// own user), but it must not start a unit: that would invent a workspace
	// path for nobody, and the instance would be unowned.
	if r.opts.User == "" {
		return nil, errors.New("this runner has no user: it can reconcile and reap instances, but not start them")
	}
	if len(t.RequiresStorages) > 0 && r.opts.Storages == nil {
		return nil, fmt.Errorf("tool %s requires storages %v but no StorageProvider is configured",
			t.ID, t.RequiresStorages)
	}

	// The named environment: its root is mounted read-only (in mountSpec) and
	// its variables go between the platform defaults and the tool's own. An
	// undeclared reference is a hard error — the alternative is a unit that
	// starts and then cannot find its interpreter.
	var envEnv []string
	if t.Environment != "" {
		if r.opts.Environments == nil {
			return nil, fmt.Errorf("tool %s declares environment %q but no environment provider is configured on this host",
				t.ID, t.Environment)
		}
		e, ok := r.opts.Environments.Get(t.Environment)
		if !ok {
			return nil, fmt.Errorf("tool %s declares environment %q, which is not declared in %s",
				t.ID, t.Environment, config.EnvironmentsPath(r.opts.ConfigDir))
		}
		envEnv = e.Env
	}

	paths := PathsFor(r.opts.ConfigDir, r.opts.User, t.ID, jobID)
	if err := paths.EnsureDirs(t); err != nil {
		return nil, err
	}

	view := NewPathView(t.Sandbox, paths, config.GatewayAPIBase(r.opts.ConfigDir), envEnv)

	spec, err := r.mountSpec(t, paths)
	if err != nil {
		return nil, err
	}

	res := t.Resources
	if j != nil {
		res = job.EffectiveResources(j, t)
	}
	return &prepared{
		tool:       t,
		paths:      paths,
		view:       view,
		spec:       spec,
		env:        view.Env(t, j),
		jobDir:     paths.JobDir,
		jobCommand: jobCommandOf(j),
		limiter:    BuildLimiter(res),
	}, nil
}

// mountSpec builds the sandbox view for one unit.
func (r *Runner) mountSpec(t *tool.Tool, p Paths) (*sandbox.Spec, error) {
	spec, err := BuildSpec(t, p, r.opts.Storages)
	if err != nil {
		return nil, err
	}
	// The named environment's root: read-only, at its own host path. Added here
	// rather than inside BuildSpec because the read side never resolves into it
	// (see environment.MountsFor).
	if t.Environment != "" && r.opts.Environments != nil {
		if e, ok := r.opts.Environments.Get(t.Environment); ok {
			if err := environment.MountsFor(spec, []environment.Environment{e}); err != nil {
				return nil, err
			}
		}
	}
	return spec, nil
}

// BuildSpec is the mount table for one unit: builtin (workspace, virtual home,
// tool package) → environment → data storages.
//
// Every entry is an exact path. Binding a parent would hand the sandbox
// everything under it that is group-readable (see package sandbox docs).
//
// It is exported because the read-only side needs the same mapping to turn a
// sandbox path back into a host path (an instance's artifacts, a file read):
// the mount table is the single definition of "what a tool can see", and a
// second implementation would be a second answer to a security question.
func BuildSpec(t *tool.Tool, p Paths, storages storage.Provider) (*sandbox.Spec, error) {
	spec := &sandbox.Spec{}
	for _, m := range []sandbox.Mount{
		{HostPath: p.Workspace, SandboxPath: sandbox.PathWorkspace, Mode: sandbox.ReadWrite, Origin: "builtin"},
		{HostPath: p.Home, SandboxPath: sandbox.HomePath(p.User), Mode: sandbox.ReadWrite, Origin: "builtin"},
		{HostPath: p.FlowRuns, SandboxPath: sandbox.PathFlow, Mode: sandbox.ReadWrite, Origin: "builtin"},
		{HostPath: t.Dir, SandboxPath: sandbox.PathTool, Mode: sandbox.ReadOnly, Origin: "tool"},
	} {
		if err := spec.Add(m); err != nil {
			return nil, err
		}
	}

	// Environment mounts: host paths the tool author declared (conda trees,
	// module prefixes, .sif overlays). Read-only, and deliberately allowed to
	// be coarse because they hold no user data (AGENTS.md: 环境与数据要分开).
	for _, rm := range t.ROMounts {
		if !strings.HasPrefix(rm.SandboxPath, "/") {
			return nil, fmt.Errorf("ro_mounts: sandbox_path %q must be absolute", rm.SandboxPath)
		}
		if sandbox.SandboxPathIsReserved(rm.SandboxPath) {
			return nil, fmt.Errorf("ro_mounts: sandbox_path %q is inside a read-only system directory "+
				"(/usr /bin /lib* are bound read-only, so bubblewrap cannot create a mount point there) — "+
				"mount into %s instead, which is first on the sandbox PATH", rm.SandboxPath, sandbox.PathToolBin)
		}
		if _, err := os.Stat(rm.Host); err != nil {
			return nil, fmt.Errorf("ro_mounts: host %q is not readable: %w", rm.Host, err)
		}
		if err := spec.Add(sandbox.Mount{
			HostPath: rm.Host, SandboxPath: rm.SandboxPath, Mode: sandbox.ReadOnly, Origin: "env",
		}); err != nil {
			return nil, err
		}
	}

	// Data storages: the closure of ADR-020. What the tool declares here is
	// exactly what a `type: path` parameter may select from, so the three sets
	// stay identical by construction.
	if len(t.RequiresStorages) > 0 {
		if storages == nil {
			return nil, fmt.Errorf("tool %s requires storages %v but no StorageProvider is configured",
				t.ID, t.RequiresStorages)
		}
		sts, err := storages.ForTool(t.RequiresStorages)
		if err != nil {
			return nil, err
		}
		if err := storage.MountsFor(spec, sts); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

// ─────────────────────────────────────────────────────────────────────────
// task
// ─────────────────────────────────────────────────────────────────────────

// RunTask executes a submitted job to completion.
//
// A non-zero exit from the tool is *data*, not an error: it is recorded on the
// instance and returned with a nil error. Callers distinguish "SRCOS could not
// run this" (error) from "the tool ran and failed" (state == failed).
func (r *Runner) RunTask(ctx context.Context, t *tool.Tool, loaded *job.Loaded) (*Instance, error) {
	if t.Kind != tool.KindTask {
		return nil, fmt.Errorf("tool %s is kind %s; RunTask only handles %s", t.ID, t.Kind, tool.KindTask)
	}
	if loaded.ID == "" {
		return nil, errors.New("job has no id (the job directory name)")
	}

	prep, err := r.prepare(t, loaded.Job, loaded.ID)
	if err != nil {
		return nil, err
	}

	inst := &Instance{
		ID:         InstanceID(r.opts.User, t.ID, loaded.ID),
		User:       r.opts.User,
		Tool:       t.ID,
		ToolDigest: t.Digest,
		Kind:       string(t.Kind),
		JobName:    loaded.Job.Name,
		State:      StatePending,
		Backend:    string(t.Backend),
		Sandbox:    string(t.Sandbox),
		LogPath:    prep.paths.LogPath,
		WorkDir:    prep.paths.JobDir,
		Outputs:    loaded.Job.Outputs,
		Tags:       loaded.Job.Tags,
		StartedAt:  time.Now().UTC(),
	}
	fail := func(format string, a ...any) *Instance {
		inst.State = StateFailed
		inst.Error = fmt.Sprintf(format, a...)
		inst.EndedAt = time.Now().UTC()
		_ = SaveInstance(prep.paths.RecordPath, inst)
		return inst
	}

	backend, err := r.backendFor(t)
	if err != nil {
		return fail("%v", err), nil
	}

	argv, err := prep.argvFor(t)
	if err != nil {
		return fail("%v", err), nil
	}
	req := StartRequest{
		Tool:       t,
		Job:        loaded.Job,
		Spec:       prep.spec,
		Paths:      prep.paths,
		View:       prep.view,
		Argv:       argv,
		Cwd:        prep.view.Cwd(),
		Env:        prep.env,
		Limiter:    prep.limiter,
		UnitName:   UnitName(inst.ID),
		LogPath:    prep.paths.LogPath,
		InstanceID: inst.ID,
	}

	h, err := backend.Start(ctx, req)
	if err != nil {
		return fail("%v", err), nil
	}
	inst.BackendRef = h.Ref()
	if pr, ok := h.(PidReporter); ok {
		// A task needs the same treatment a service gets: without a user
		// systemd, the recorded pid (with the start time that pins the
		// incarnation) is the only way to stop it later — which is what
		// `flow cancel` and the admin console need.
		inst.PID, inst.PIDStart = pr.ChildPID()
	}
	inst.Command = h.Command()
	inst.Limiter = h.Limiter()
	inst.Mounts = renderMounts(prep.spec)
	inst.RequestedCPU, inst.RequestedMemory = requestedResources(t, loaded.Job)
	inst.State = StateRunning
	inst.LastActiveAt = inst.StartedAt
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		return nil, err
	}

	status := h.Wait(ctx)

	// A deliberate stop is the authority. `Cancel` / `flow cancel` / the reaper
	// run in another goroutine (or another process) and write `stopped` while this
	// waiter is blocked; letting the exit status overwrite it would report a
	// deliberate kill as a crash. The record is the shared decision point between
	// the two, so it is read back instead of assumed — and the stopped record is
	// returned as-is, because it already carries the ending.
	if persisted, err := LoadInstance(prep.paths.RecordPath); err == nil && persisted.State == StateStopped {
		return persisted, nil
	}

	inst.EndedAt = time.Now().UTC()
	inst.Duration = inst.EndedAt.Sub(inst.StartedAt).Round(time.Millisecond).String()

	switch {
	case ctx.Err() != nil:
		inst.ExitCode = status.Code
		inst.State = StateFailed
		inst.Error = "cancelled: " + ctx.Err().Error()
	default:
		applyExitStatus(inst, status)
	}

	// A doneWhen probe defers the real verdict: exit 0 then only means
	// "submitted", and success is decided by the probe (ADR-006).
	if inst.State == StateSucceeded && loaded.Job.DoneWhen != nil {
		if err := r.waitForProbe(ctx, loaded.Job.DoneWhen, prep, inst); err != nil {
			inst.State = StateFailed
			inst.Error = err.Error()
		}
	}

	inst.Outputs = r.reportOutputs(prep.spec, loaded.Job.Outputs)
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		return nil, err
	}
	r.auditInstance(inst, "instance.done", map[string]any{"state": string(inst.State), "exit": inst.ExitCode})
	return inst, nil
}

// waitForProbe polls a doneWhen probe until it is satisfied.
//
// The timeout is the job's doneWhenTimeout, falling back to the tool's
// walltime: a probe that never fires must not hold a slot forever.
func (r *Runner) waitForProbe(ctx context.Context, dw *tool.DoneWhen, prep *prepared, inst *Instance) error {
	if dw.Type == tool.DoneWhenExitCode || dw.Path == "" {
		return nil
	}
	timeout := 12 * time.Hour
	if prep.tool.Completion != nil && prep.tool.Completion.DoneWhenTimeout != "" {
		if secs, err := tool.ParseWalltime(prep.tool.Completion.DoneWhenTimeout); err == nil && secs > 0 {
			timeout = time.Duration(secs) * time.Second
		}
	}

	host, _, err := prep.spec.Resolve(dw.Path)
	if err != nil {
		return fmt.Errorf("doneWhen path %q: %w", dw.Path, err)
	}

	deadline := time.Now().Add(timeout)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if probeSatisfied(dw.Type, host) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("doneWhen probe %s(%s) not satisfied within %s", dw.Type, dw.Path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func probeSatisfied(kind tool.DoneWhenType, host string) bool {
	fi, err := os.Stat(host)
	switch kind {
	case tool.DoneWhenFileExists:
		return err == nil
	case tool.DoneWhenDirNonEmpty:
		if err != nil || !fi.IsDir() {
			return false
		}
		entries, err := os.ReadDir(host)
		return err == nil && len(entries) > 0
	default:
		return err == nil
	}
}

// ─────────────────────────────────────────────────────────────────────────
// service
// ─────────────────────────────────────────────────────────────────────────

// StartService brings up a long-running unit, waits for its healthcheck, and
// publishes a route for the proxy.
//
// The order matters: the port is acquired before the process starts (so the
// tool can bind it), the route is published only after the healthcheck passes
// (so the proxy never forwards into a half-started process), and every failure
// path releases the port.
func (r *Runner) StartService(ctx context.Context, t *tool.Tool, j *job.Job) (*Instance, error) {
	if t.Kind != tool.KindService {
		return nil, fmt.Errorf("tool %s is kind %s; StartService only handles %s", t.ID, t.Kind, tool.KindService)
	}
	if t.Ingress == nil {
		return nil, fmt.Errorf("tool %s declares kind: service without ingress", t.ID)
	}
	pool, err := r.portPool()
	if err != nil {
		return nil, err
	}

	prep, err := r.prepare(t, j, "")
	if err != nil {
		return nil, err
	}

	inst := newServiceInstance(t, prep, j)
	fail := func(format string, a ...any) *Instance {
		inst.State = StateFailed
		inst.Error = fmt.Sprintf(format, a...)
		inst.EndedAt = time.Now().UTC()
		_ = SaveInstance(prep.paths.RecordPath, inst)
		return inst
	}

	// An external backend runs nothing: forwarding has no argv, no port pool,
	// no credential and no ownership (design §3).
	if t.Backend == tool.BackendExternal {
		return r.startExternal(ctx, t, j, prep)
	}

	backend, err := r.backendFor(t)
	if err != nil {
		return fail("%v", err), nil
	}

	port, err := pool.Acquire(inst.ID)
	if err != nil {
		return fail("no free port: %v", err), nil
	}
	// Every exit from here on either releases the port or keeps the instance.
	keepPort := false
	released := false
	defer func() {
		if !released && !keepPort {
			pool.Release(port)
		}
	}()
	inst.Endpoint = route.Target{Host: "127.0.0.1", Port: port}.String()

	// The tool is told which port to bind; without this the port pool and the
	// tool would have to agree by convention. The environment is completed first
	// because a declarative command may reference it as ${SRCOS_PORT}.
	prep.env = append(prep.env, "SRCOS_PORT="+strconv.Itoa(port))
	argv, err := prep.argvFor(t)
	if err != nil {
		return fail("resolve command: %v", err), nil
	}

	req := StartRequest{
		Tool:         t,
		Job:          j,
		Spec:         prep.spec,
		Paths:        prep.paths,
		View:         prep.view,
		Argv:         argv,
		Cwd:          prep.view.Cwd(),
		Env:          prep.env,
		Limiter:      prep.limiter,
		UnitName:     UnitName(inst.ID),
		LogPath:      prep.paths.LogPath,
		InstanceID:   inst.ID,
		WantEndpoint: true,
	}

	h, err := backend.Start(ctx, req)
	if err != nil {
		return fail("%v", err), nil
	}
	inst.BackendRef = h.Ref()
	if pr, ok := h.(PidReporter); ok {
		// Degraded mode: SRCOS owns the process, so its pid — with the start
		// time that pins the incarnation — is what a later `svc stop`, or a
		// reaper running in another process, has to work with.
		inst.PID, inst.PIDStart = pr.ChildPID()
	}
	inst.Command = h.Command()
	inst.Limiter = h.Limiter()
	// A backend that publishes its own endpoint is authoritative about where it
	// is reachable: the local pool's guess does not apply (an SGE job tunnels to
	// a compute node; an external backend is declared elsewhere). The pool
	// reservation is handed back rather than held for a port nothing listens on.
	if h.WantEndpoint() {
		ep, ok := h.Endpoint()
		if !ok {
			_ = h.Stop(context.Background())
			return fail("backend %s did not publish an endpoint", backend.Name()), nil
		}
		inst.Endpoint = ep.String()
		pool.Release(port)
		released = true
	}
	inst.Mounts = renderMounts(prep.spec)
	inst.RequestedCPU, inst.RequestedMemory = requestedResources(t, j)
	inst.State = StateStarting
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		_ = h.Stop(context.Background())
		return nil, err
	}

	if err := r.healthcheck(ctx, t, h, port, prep); err != nil {
		_ = h.Stop(context.Background())
		return fail("healthcheck: %v", err), nil
	}

	inst.State = StateRunning
	inst.Healthcheck = "ok"
	// The agent's credential, before the route is published: a hosted agent that
	// cannot be given one must not be reported as a running service (A1).
	if err := r.issueAgentToken(t, inst, prep); err != nil {
		_ = h.Stop(context.Background())
		return fail("agent credential: %v", err), nil
	}
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		_ = h.Stop(context.Background())
		return nil, err
	}

	target, err := route.ParseTarget(inst.Endpoint)
	if err != nil {
		_ = h.Stop(context.Background())
		return fail("%v", err), nil
	}
	if err := r.publishRoute(t, inst, target); err != nil {
		_ = h.Stop(context.Background())
		return fail("publish route: %v", err), nil
	}

	keepPort = true
	return inst, nil
}

// newServiceInstance is the record both service paths start from — the one
// SRCOS instantiates and the one it forwards to. They differ in where the
// endpoint comes from, not in what an instance *is*.
func newServiceInstance(t *tool.Tool, prep *prepared, j *job.Job) *Instance {
	inst := &Instance{
		ID:         InstanceID(prep.paths.User, t.ID, ""),
		User:       prep.paths.User,
		Tool:       t.ID,
		ToolDigest: t.Digest,
		Kind:       string(t.Kind),
		JobName:    t.Name,
		State:      StatePending,
		Backend:    string(t.Backend),
		Sandbox:    string(t.Sandbox),
		LogPath:    prep.paths.LogPath,
		WorkDir:    prep.paths.Workspace,
		RoutePath:  route.DefaultPath(prep.paths.User, t.ID),
		StartedAt:  time.Now().UTC(),
	}
	inst.LastActiveAt = inst.StartedAt
	if j != nil {
		inst.Tags = j.Tags
	}
	return inst
}

// startExternal forwards to a backend that already runs (design §3).
//
// It shares everything it can with StartService — the record, the healthcheck,
// the route — and differs only where "nothing is started" matters: no port
// pool, no argv, no credential, and no ownership.
func (r *Runner) startExternal(ctx context.Context, t *tool.Tool, j *job.Job, prep *prepared) (*Instance, error) {
	inst := newServiceInstance(t, prep, j)
	fail := func(format string, a ...any) *Instance {
		inst.State = StateFailed
		inst.Error = fmt.Sprintf(format, a...)
		inst.EndedAt = time.Now().UTC()
		_ = SaveInstance(prep.paths.RecordPath, inst)
		return inst
	}

	backend, err := r.backendFor(t)
	if err != nil {
		return fail("%v", err), nil
	}
	h, err := backend.Start(ctx, StartRequest{
		Tool:         t,
		Job:          j,
		Spec:         prep.spec,
		Paths:        prep.paths,
		View:         prep.view,
		LogPath:      prep.paths.LogPath,
		InstanceID:   inst.ID,
		WantEndpoint: true,
	})
	if err != nil {
		return fail("%v", err), nil
	}
	target, ok := h.Endpoint()
	if !ok {
		return fail("backend %s did not publish an endpoint", backend.Name()), nil
	}
	inst.Endpoint = target.String()
	inst.Limiter = h.Limiter()
	inst.State = StateStarting
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		return nil, err
	}

	path, timeout := healthcheckSpec(t)
	if err := probeHTTP(ctx, target, path, timeout, h); err != nil {
		return fail("healthcheck: %v", err), nil
	}

	inst.State = StateRunning
	inst.Healthcheck = "ok"
	if err := r.publishRoute(t, inst, target); err != nil {
		return fail("publish route: %v", err), nil
	}
	if err := SaveInstance(prep.paths.RecordPath, inst); err != nil {
		return nil, err
	}
	r.auditInstance(inst, "instance.forwarded", map[string]any{"endpoint": target.String()})
	return inst, nil
}

// publishRoute puts one instance into the routing table the proxy reads.
func (r *Runner) publishRoute(t *tool.Tool, inst *Instance, target route.Target) error {
	if r.opts.Routes == nil {
		return nil
	}
	return r.opts.Routes.Put(route.Entry{
		User:        inst.User,
		Tool:        inst.Tool,
		InstanceID:  inst.ID,
		Path:        inst.RoutePath,
		Target:      target,
		WebSocket:   t.Ingress.WebSocketEnabled(),
		BWLimit:     t.Ingress.BWLimit,
		BackendPath: t.Ingress.BackendPath,
		State:       string(inst.State),
	})
}

// issueAgentToken mints the credential a hosted agent uses (A1) and hands it to
// the sandbox as a file in the virtual home.
//
// Not an environment variable: every instance runs as the same OS user, so a
// token in /proc/<pid>/environ is readable from anywhere in that uid's view,
// while a 0600 file is exactly the instance's own. The agent reads
// $HOME/.srcos/agent-token.
func (r *Runner) issueAgentToken(t *tool.Tool, inst *Instance, prep *prepared) error {
	if t.Agent == nil {
		return nil
	}
	if r.opts.AgentTokens == nil {
		return errors.New("this tool hosts an agent but no agent-token store is wired on this host")
	}
	plaintext, err := r.opts.AgentTokens.MintInstance(inst.User, inst.ID, t.Agent.MCP, t.Agent.Tools)
	if err != nil {
		return err
	}
	dir := filepath.Join(prep.paths.Home, ".srcos")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-token"), []byte(plaintext+"\n"), 0o600); err != nil {
		return err
	}
	r.auditInstance(inst, "agenttoken.mint", map[string]any{
		"scopes": strings.Join(t.Agent.MCP, ","),
		"tools":  strings.Join(t.Agent.Tools, ","),
	})
	return nil
}

// healthcheck polls the instance's ingress until it answers, because a process
// that has started is not a service that is ready (Jupyter needs 5-15s).
func (r *Runner) healthcheck(ctx context.Context, t *tool.Tool, h Handle, port int, prep *prepared) error {
	path, timeout := healthcheckSpec(t)

	// A backend that publishes its own endpoint (SGE, over a rendezvous file)
	// is authoritative: the local port pool's guess does not apply on a
	// compute node.
	if h.WantEndpoint() {
		ep, ok := h.Endpoint()
		if !ok {
			return errors.New("backend did not publish an endpoint")
		}
		return probeHTTP(ctx, ep, path, timeout, h)
	}
	return probeHTTP(ctx, route.Target{Host: "127.0.0.1", Port: port}, path, timeout, h)
}

// healthcheckSpec is the probe path and deadline a tool declares, with the
// defaults an app server needs (it can take a minute to load its libraries).
func healthcheckSpec(t *tool.Tool) (string, time.Duration) {
	path := "/"
	timeout := 120 * time.Second
	if hc := t.Ingress.Healthcheck; hc != nil {
		if hc.Path != "" {
			path = hc.Path
		}
		if hc.Timeout != "" {
			if secs, err := tool.ParseWalltime(hc.Timeout); err == nil && secs > 0 {
				timeout = time.Duration(secs) * time.Second
			}
		}
	}
	return path, timeout
}

func probeHTTP(ctx context.Context, target route.Target, path string, timeout time.Duration, h Handle) error {
	url := "http://" + target.String() + path
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if !h.Alive() {
			return fmt.Errorf("process exited before it became ready (last probe: %v)", lastErr)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			// Any HTTP answer means the server is listening and speaking; a
			// 401/403 is a login page, which is a perfectly healthy service.
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("not ready after %s: %v", timeout, lastErr)
}

// StopService stops a service instance and withdraws its route.
func (r *Runner) StopService(ctx context.Context, t *tool.Tool, inst *Instance) error {
	if !inst.State.Terminal() {
		inst.State = StateStopping
		_ = SaveInstance(InstancePath(r.opts.ConfigDir, inst.ID), inst)
	}

	// Withdraw the route first: a request arriving mid-stop should get a
	// "starting" page, not a 502 into a dying process.
	if r.opts.Routes != nil {
		r.opts.Routes.DeleteInstance(inst.User, inst.Tool, inst.ID)
	}

	var err error
	if b, ok := r.opts.Backends[inst.Backend]; ok {
		if stopper, ok := b.(UnitStopper); ok {
			err = stopper.StopUnit(ctx, inst)
		}
	}
	if r.ports != nil {
		if _, port, splitErr := splitEndpoint(inst.Endpoint); splitErr == nil {
			r.ports.Release(port)
		}
	}

	inst.State = StateStopped
	inst.EndedAt = time.Now().UTC()
	inst.Duration = inst.EndedAt.Sub(inst.StartedAt).Round(time.Millisecond).String()
	if err != nil {
		inst.Error = "stop: " + err.Error()
	}
	// The credential's lifetime is the instance's: revoke it on the way out,
	// whichever way the instance is leaving (explicit stop, the reaper, an admin
	// stopping someone else's service).
	if r.opts.AgentTokens != nil {
		if _, rerr := r.opts.AgentTokens.RevokeInstance(inst.ID); rerr != nil {
			log.Printf("[srcos] revoke instance credential %s: %v", inst.ID, rerr)
		} else {
			_ = os.Remove(filepath.Join(config.HomeDir(r.opts.ConfigDir, inst.User), ".srcos", "agent-token"))
			r.auditInstance(inst, "agenttoken.revoke", nil)
		}
	}
	return SaveInstance(InstancePath(r.opts.ConfigDir, inst.ID), inst)
}

// UnitStopper is implemented by backends that can stop a unit without holding
// the live Handle: the reaper, `svc stop`, and startup reconciliation all run
// in a process that may not be the one that started the unit (and after a
// restart, no process holds it at all).
//
// It receives the whole record because a backend may not have a name to work
// with: in the degraded mode there is no unit, only the pid SRCOS started.
type UnitStopper interface {
	StopUnit(ctx context.Context, inst *Instance) error
}

// ─────────────────────────────────────────────────────────────────────────
// 回收
// ─────────────────────────────────────────────────────────────────────────

// Reaper enforces service lifecycle limits.
//
// Idle reaping is intentionally conservative: HPC queue wait makes a restart
// expensive, so the reaper only stops a service that is genuinely idle, and
// never one with an open WebSocket (a notebook the user left open is not idle
// even if it is quiet).
type Reaper struct {
	Runner *Runner
	// ActiveWS reports whether an instance currently has live connections.
	ActiveWS func(instanceID string) bool
	// LastActive returns the newest observed activity for an instance, or the
	// zero time when the caller has none to offer.
	//
	// It exists because the *record* only knows when the instance started:
	// activity is only observable where the traffic is, which is the proxy (the
	// gateway's, or a second process's). Without this, idleTTL would measure
	// "time since start" and a notebook someone has been working in all
	// afternoon would be reaped mid-thought.
	LastActive func(instanceID string) time.Time
}

// Sweep performs one pass and returns the instances it stopped.
func (rp *Reaper) Sweep(ctx context.Context, now time.Time) ([]string, error) {
	if rp.Runner.ports == nil {
		return nil, errors.New("reaper needs the port pool to release ports")
	}
	insts, err := ListInstances(rp.Runner.opts.ConfigDir)
	if err != nil {
		return nil, err
	}
	var stopped []string
	var problems []string
	for _, inst := range insts {
		if !isReapable(inst) {
			continue
		}
		t, err := findTool(rp.Runner.opts.ToolsDir, inst.Tool)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", inst.ID, err))
			continue
		}
		if t.Lifecycle == nil {
			continue
		}

		reason := ""
		if maxLife := parseDurationOr(t.Lifecycle.MaxLifetime, 0); maxLife > 0 &&
			now.Sub(inst.StartedAt) > maxLife {
			reason = "maxLifetime exceeded"
		}
		if reason == "" {
			idleTTL := parseDurationOr(t.Lifecycle.IdleTTL, 0)
			// A backend on a cluster queues for minutes to hours, so idle
			// reaping is opt-in there (ADR-015).
			if t.Backend != tool.BackendSGE && idleTTL > 0 {
				lastActive := inst.LastActiveAt
				if rp.LastActive != nil {
					if observed := rp.LastActive(inst.ID); observed.After(lastActive) {
						lastActive = observed
					}
				}
				if !lastActive.IsZero() && now.Sub(lastActive) > idleTTL {
					if rp.ActiveWS != nil && rp.ActiveWS(inst.ID) {
						continue // open WebSocket: not idle, whatever the clock says
					}
					reason = "idle past idleTTL"
				}
			}
		}
		if reason == "" {
			continue
		}
		if err := rp.Runner.StopService(ctx, t, inst); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", inst.ID, err))
			continue
		}
		stopped = append(stopped, inst.ID+" ("+reason+")")
		rp.Runner.auditInstance(inst, "instance.reaped", map[string]any{"reason": reason})
	}
	if len(problems) > 0 {
		return stopped, errors.New(strings.Join(problems, "; "))
	}
	return stopped, nil
}

// Reconcile brings the records and the routing table back in line with reality
// after a SRCOS restart.
//
// It is the counterpart of the reaper: the reaper ends things that should be
// gone, Reconcile adopts things that are still there. Without it, a restart
// would either lose every live service's route or leave orphan processes with
// no record.
func (r *Runner) Reconcile(ctx context.Context) (adopted, orphaned []string, err error) {
	if r.ports == nil {
		return nil, nil, errors.New("reconcile needs the port pool")
	}
	insts, err := ListInstances(r.opts.ConfigDir)
	if err != nil {
		return nil, nil, err
	}
	var problems []string
	for _, inst := range insts {
		if inst.Kind != string(tool.KindService) {
			continue
		}
		// The tool package has to still exist: reconcile needs its record to be
		// meaningful, and a missing package is a problem worth reporting rather
		// than something to silently adopt.
		if _, terr := findTool(r.opts.ToolsDir, inst.Tool); terr != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", inst.ID, terr))
			continue
		}
		_, port, perr := splitEndpoint(inst.Endpoint)
		if perr != nil {
			continue
		}

		alive := false
		if b, ok := r.opts.Backends[inst.Backend]; ok {
			if prober, ok := b.(UnitProber); ok {
				alive = prober.UnitAlive(ctx, inst)
			}
		}

		switch {
		case alive && !inst.State.Terminal():
			// Still running: re-pin its port so it is not handed to a second
			// instance, and re-publish the route the in-memory table lost.
			if r.ports != nil {
				if err := r.ports.Reserve(port, inst.ID); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", inst.ID, err))
					continue
				}
			}
			if r.opts.Routes != nil {
				_ = r.opts.Routes.Put(route.Entry{
					User:       inst.User,
					Tool:       inst.Tool,
					InstanceID: inst.ID,
					Path:       inst.RoutePath,
					Target:     route.Target{Host: "127.0.0.1", Port: port},
					WebSocket:  true,
					State:      string(inst.State),
				})
			}
			adopted = append(adopted, inst.ID)
			r.auditInstance(inst, "instance.adopted", nil)

		case !alive && !inst.State.Terminal():
			// The record says running but nothing is: mark it stopped so the
			// user sees the truth instead of a card that 502s.
			inst.State = StateStopped
			inst.EndedAt = time.Now().UTC()
			inst.Error = "found stopped during startup reconcile"
			_ = SaveInstance(InstancePath(r.opts.ConfigDir, inst.ID), inst)
			if r.ports != nil {
				r.ports.Release(port)
			}
			if r.opts.Routes != nil {
				r.opts.Routes.DeleteInstance(inst.User, inst.Tool, inst.ID)
			}
			orphaned = append(orphaned, inst.ID)
			r.auditInstance(inst, "instance.orphaned", map[string]any{"error": inst.Error})
		}
	}
	sort.Strings(adopted)
	sort.Strings(orphaned)
	if len(problems) > 0 {
		err = errors.New(strings.Join(problems, "; "))
	}
	return adopted, orphaned, err
}

// ReconcileTasks settles task records that no live process backs any more.
//
// A task's verdict (exit code, duration) is written by the SRCOS process that
// started it. If that process is gone — a restart, a crash — nobody will ever
// write it, and without this the record would say "running" forever: a lie in
// the UI *and* a quota leak, because a running instance counts against its
// user's ceiling.
//
// It runs on the scan tick rather than only at startup, because the process
// usually outlives the restart: the truth only becomes knowable when it dies.
//
// The verdict itself is asked of the backend (TaskProber): a systemd unit
// records its own exit status in a file on stop, so a run interrupted by a
// restart is settled with its real outcome. Only a backend that cannot answer
// leaves the record as `stopped` with an explanation — never as a guess.
func (r *Runner) ReconcileTasks(ctx context.Context) (adopted, settled []string, err error) {
	insts, err := ListInstances(r.opts.ConfigDir)
	if err != nil {
		return nil, nil, err
	}
	for _, inst := range insts {
		if inst.Kind != string(tool.KindTask) {
			continue
		}
		switch inst.State {
		case StatePending, StateSucceeded, StateFailed, StateStopped:
			// `pending` is a *queue position*, not a process: the task queue owns
			// it and will start it. The rest are already decided.
			continue
		}

		alive := false
		if b, ok := r.opts.Backends[inst.Backend]; ok {
			if prober, ok := b.(UnitProber); ok {
				alive = prober.UnitAlive(ctx, inst)
			}
		}
		if alive {
			// Still running after the restart: leave it alone. A later tick will
			// see it die and settle it then.
			adopted = append(adopted, inst.ID)
			continue
		}

		inst.EndedAt = time.Now().UTC()
		if !inst.StartedAt.IsZero() {
			inst.Duration = inst.EndedAt.Sub(inst.StartedAt).Round(time.Millisecond).String()
		}

		// Ask the backend for the run's own account of how it ended before
		// concluding that there is none: a unit records its exit status itself
		// (ADR-022), so the common case after a restart is a real verdict — not
		// an apology.
		verdict, haveVerdict := ExitStatus{}, false
		if b, ok := r.opts.Backends[inst.Backend]; ok {
			if prober, ok := b.(TaskProber); ok {
				verdict, haveVerdict = prober.TaskExit(ctx, inst)
			}
		}
		if haveVerdict {
			applyExitStatus(inst, verdict)
		} else {
			inst.State = StateStopped
			if inst.Error == "" {
				inst.Error = "no verdict: the process is gone, and neither systemd nor a waiter recorded " +
					"an exit status. The log and any outputs are still there; re-run with " +
					"`srcos job run --force` if you need a fresh verdict."
			}
		}
		if serr := SaveInstance(InstancePath(r.opts.ConfigDir, inst.ID), inst); serr != nil {
			err = serr
			continue
		}
		settled = append(settled, inst.ID)
		r.auditInstance(inst, "instance.settled", map[string]any{"state": string(inst.State), "exit": inst.ExitCode})
	}
	sort.Strings(adopted)
	sort.Strings(settled)
	return adopted, settled, err
}

// applyExitStatus writes how a run ended onto its record.
//
// It is shared by the waiter (RunTask) and the reconciler (ReconcileTasks), so a
// task settled after a restart is described exactly like one settled live — the
// wording of a failure is part of what a user reads, and two spellings of it
// would be two facts.
func applyExitStatus(inst *Instance, status ExitStatus) {
	inst.ExitCode = status.Code
	if status.Code == 0 {
		inst.State = StateSucceeded
		inst.Error = ""
		return
	}
	inst.State = StateFailed
	inst.Error = fmt.Sprintf("tool exited with code %d", status.Code)
	if status.Code < 0 && status.Err != nil {
		inst.Error = status.Err.Error()
	}
}

// auditInstance records one lifecycle decision the platform made about an
// instance (ADR-024). The actor is the instance's user with kind "system": an
// auditor reading "why did alice's service disappear" wants these next to her
// own acts. A nil recorder is a no-op.
func (r *Runner) auditInstance(inst *Instance, action string, params map[string]any) {
	if inst == nil {
		return
	}
	r.opts.Audit.Record(audit.NewEvent(audit.Actor{User: inst.User, Kind: audit.KindSystem}, action).
		WithTarget("instance", inst.ID, "").
		WithDigest(inst.ToolDigest).
		WithParams(params).
		WithRefs(map[string]string{"tool": inst.Tool}).
		Allowed())
}

// TaskProber is implemented by backends that can report how a *finished* task
// ended after the fact — when the process that started it is no longer around
// to observe the exit.
//
// It is what turns "no verdict" into a real one after a SRCOS restart: the
// local backend reads the file systemd's ExecStopPost wrote (ADR-022), which
// survives both the restart and garbage collection of the unit.
type TaskProber interface {
	// TaskExit returns the run's exit status, or ok=false when no verdict was
	// recorded (never a guess).
	TaskExit(ctx context.Context, inst *Instance) (ExitStatus, bool)
}

// UnitProber is implemented by backends that can answer "is this unit still
// running?" from a persisted record — which is all that survives a SRCOS
// restart. A backend with a name to probe (a systemd unit, an SGE job) uses
// it; the degraded mode has only the pid SRCOS recorded.
type UnitProber interface {
	UnitAlive(ctx context.Context, inst *Instance) bool
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

// UnitName derives a backend unit name from an instance id.
//
// Instance ids are built from a validated username, a validated tool id and a
// slugified job name, so the result is already [A-Za-z0-9._-]; the prefix keeps
// SRCOs units recognisable in `systemctl --user list-units` and `qstat`.
func UnitName(instanceID string) string { return "srcos-" + sanitizeUnitName(instanceID) }

func sanitizeUnitName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	// systemd unit names are limited; keep the tail (the job slug) readable.
	if len(out) > 120 {
		out = out[len(out)-120:]
	}
	if out == "" {
		out = "unit"
	}
	return out
}

func splitEndpoint(endpoint string) (string, int, error) {
	if endpoint == "" {
		return "", 0, errors.New("empty endpoint")
	}
	host, portStr, err := splitHostPort(endpoint)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("endpoint %q has no port", s)
	}
	return s[:i], s[i+1:], nil
}

func isReapable(inst *Instance) bool {
	if inst.Kind != string(tool.KindService) {
		return false
	}
	// A forwarded backend is not SRCOS's process: idleTTL / maxLifetime describe
	// a lifetime the platform owns, and it owns nothing here.
	if inst.Backend == string(tool.BackendExternal) {
		return false
	}
	switch inst.State {
	case StateRunning, StateIdle, StateStarting:
		return true
	}
	return false
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	// Accept the H:MM:SS spelling used by walltime.
	if secs, err := tool.ParseWalltime(s); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}

func findTool(toolsDir, id string) (*tool.Tool, error) {
	return tool.Find(toolsDir, id)
}

// requestedResources is what the unit actually asked for, after job overrides.
func requestedResources(t *tool.Tool, j *job.Job) (int, string) {
	res := t.Resources
	if j != nil {
		res = job.EffectiveResources(j, t)
	}
	return res.CPU, res.Memory
}

func renderMounts(spec *sandbox.Spec) []string {
	var out []string
	for _, m := range spec.Mounts() {
		out = append(out, fmt.Sprintf("%s:%s:%s:%s", m.HostPath, m.SandboxPath, m.Mode, m.Origin))
	}
	return out
}

// reportOutputs resolves declared output paths and annotates which exist,
// keeping "declared" and "produced" distinct in the user surface.
func (r *Runner) reportOutputs(spec *sandbox.Spec, declared []string) []string {
	out := make([]string, 0, len(declared))
	for _, d := range declared {
		host, _, err := spec.Resolve(d)
		if err != nil {
			out = append(out, d+" (unresolvable)")
			continue
		}
		if _, err := os.Stat(host); err != nil {
			out = append(out, d+" (missing)")
			continue
		}
		out = append(out, d)
	}
	return out
}

// JobManifestPath is where a service's effective parameters are recorded, so a
// restarted SRCOS can restart the service with the same inputs.
func JobManifestPath(configDir, user, toolID string) string {
	return filepath.Join(config.WorkspaceDir(configDir, user, toolID), "srcos-service.json")
}

// WriteServiceManifest persists a service's parameters next to its workspace.
func WriteServiceManifest(configDir, user, toolID string, j *job.Job) error {
	path := JobManifestPath(configDir, user, toolID)
	if j == nil {
		return os.Remove(path)
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// PortPoolDefault is the range service instances are placed in.
func PortPoolDefault() (int, int) { return portpool.DefaultLow, portpool.DefaultHigh }
