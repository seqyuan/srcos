// Package runtime executes a validated (Tool, Job) pair as a RunUnit.
//
// Phase 1 implements the `local` backend only: one process, sandboxed with
// bubblewrap, resource-limited through the OS user's own systemd scope (or
// prlimit as a fallback). The `sge` backend arrives in Phase 6.
//
// Everything that decides *what the sandbox looks like* lives in package
// sandbox; this package owns ordering, resource limiting, log capture and the
// instance record.
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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// State is the lifecycle state of an instance.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

// Instance is the persisted record of one execution of one job.
//
// ServiceInstance and TaskInstance share this table; only Kind differs
// (ADR-003). Phase 1 only produces tasks.
type Instance struct {
	ID       string `yaml:"id"`
	User     string `yaml:"user"`
	Tool     string `yaml:"tool"`
	Kind     string `yaml:"kind"`
	JobName  string `yaml:"job_name"`
	State    State  `yaml:"state"`
	ExitCode int    `yaml:"exit_code"`

	Backend string   `yaml:"backend"`
	Sandbox string   `yaml:"sandbox"`
	Limiter string   `yaml:"limiter"` // systemd-run | prlimit | none
	Command []string `yaml:"command,omitempty"`
	Mounts  []string `yaml:"mounts,omitempty"` // "host:sandbox:mode:origin"

	LogPath   string    `yaml:"log_path"`
	WorkDir   string    `yaml:"work_dir"`
	StartedAt time.Time `yaml:"started_at"`
	EndedAt   time.Time `yaml:"ended_at,omitempty"`
	Duration  string    `yaml:"duration,omitempty"`

	Outputs []string          `yaml:"outputs,omitempty"`
	Tags    map[string]string `yaml:"tags,omitempty"`
	Error   string            `yaml:"error,omitempty"`
}

// InstancePath is the deterministic record location for (user, tool, jobID).
func InstancePath(configDir, user, toolID, jobID string) string {
	return filepath.Join(config.InstancesDir(configDir), fmt.Sprintf("%s-%s-%s.yaml", user, toolID, jobID))
}

// SaveInstance writes the record atomically so a crash mid-write cannot leave
// a half-parsed YAML behind.
func SaveInstance(path string, inst *Instance) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(inst)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadInstance reads a record.
func LoadInstance(path string) (*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inst Instance
	if err := yaml.Unmarshal(data, &inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

// ListInstances returns every record under data/instances, newest first.
func ListInstances(configDir string) ([]*Instance, error) {
	entries, err := os.ReadDir(config.InstancesDir(configDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Instance
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		inst, err := LoadInstance(filepath.Join(config.InstancesDir(configDir), e.Name()))
		if err != nil {
			continue
		}
		out = append(out, inst)
	}
	sortNewestFirst(out)
	return out, nil
}

func sortNewestFirst(list []*Instance) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if a.StartedAt.After(b.StartedAt) || (a.StartedAt.Equal(b.StartedAt) && a.ID <= b.ID) {
				break
			}
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 资源限制（ADR-014：systemd-run --user 优先，prlimit 兜底）
// ─────────────────────────────────────────────────────────────────────────

type limiter struct {
	name string
	// wrap turns an inner argv into the argv that actually gets executed.
	wrap func(inner []string) []string
}

// detectLimiter picks the strongest resource control this host can provide.
//
// systemd-run --user --scope is preferred because its limits are cgroup-based
// and therefore real: MemoryMax triggers an OOM kill at the cgroup boundary
// rather than at an address-space heuristic. prlimit only bounds a process's
// own limits, which children inherit, and cannot cap total resident memory.
func detectLimiter(res tool.Resources) limiter {
	props := systemdProps(res)
	if len(props) > 0 && systemdRunUsable(props) {
		return limiter{name: "systemd-run", wrap: func(inner []string) []string {
			out := []string{"systemd-run", "--user", "--scope", "--quiet"}
			for _, p := range props {
				out = append(out, "-p", p)
			}
			return append(append(out, "--"), inner...)
		}}
	}

	if prlimit, ok := prlimitWrap(res); ok {
		return limiter{name: "prlimit", wrap: prlimit}
	}
	return limiter{name: "none", wrap: func(inner []string) []string { return inner }}
}

func systemdProps(res tool.Resources) []string {
	var props []string
	if res.CPU > 0 {
		props = append(props, fmt.Sprintf("CPUQuota=%d%%", res.CPU*100))
	}
	if res.Memory != "" {
		if b, err := tool.ParseMemory(res.Memory); err == nil {
			props = append(props, "MemoryMax="+tool.FormatMemory(b))
			// Refuse to swap instead of OOM-killing: swapping a multi-GB
			// analysis turns a fast OOM into an unbounded hang.
			props = append(props, "MemorySwapMax=0")
		}
	}
	// Fork-bomb guard. 512 is generous for a tool that fans out samples.
	props = append(props, "TasksMax=512")
	return props
}

func prlimitWrap(res tool.Resources) (func([]string) []string, bool) {
	path, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, false
	}
	var opts []string
	if res.Memory != "" {
		if b, err := tool.ParseMemory(res.Memory); err == nil {
			opts = append(opts, fmt.Sprintf("--as=%d", b))
		}
	}
	if res.CPU > 0 {
		// RLIMIT_CPU is in seconds of CPU time, not wall clock.
		opts = append(opts, fmt.Sprintf("--cpu=%d", res.CPU*3600))
	}
	opts = append(opts, "--nproc=512")

	return func(inner []string) []string {
		out := append([]string{path}, opts...)
		return append(append(out, "--"), inner...)
	}, true
}

// systemdRunUsable probes once whether `systemd-run --user --scope` accepts
// these properties here. Cached: the probe costs a process spawn and the
// answer cannot change mid-run. HPC login nodes frequently disable
// systemd --user entirely, which is exactly what this detects.
var systemdProbe struct {
	done bool
	ok   bool
}

func systemdRunUsable(props []string) bool {
	if !systemdProbe.done {
		systemdProbe.done = true
		if _, err := exec.LookPath("systemd-run"); err == nil {
			args := []string{"--user", "--scope", "--quiet"}
			for _, p := range props {
				args = append(args, "-p", p)
			}
			args = append(args, "--", "/bin/true")
			systemdProbe.ok = exec.Command("systemd-run", args...).Run() == nil
		}
	}
	return systemdProbe.ok
}

// ─────────────────────────────────────────────────────────────────────────
// Runner
// ─────────────────────────────────────────────────────────────────────────

// Local runs tools with backend: local on this host.
type Local struct {
	ConfigDir string
	User      string
	// Stdout, when set, receives a live copy of the log while the job runs.
	Stdout io.Writer
}

// pathView resolves the contract's sandbox paths into whatever the running
// process will actually see.
//
// With a mount namespace the contract's paths (/workspace, /home/<user>,
// /tool) are literally true. With `sandbox: none` there is no namespace, so
// those paths do not exist on the host and the same variables must carry real
// host paths instead. Tools never need to care: they are required to use
// $SRCOS_* rather than literal paths (tool-spec §4 规范 #2), which is exactly
// what makes the degraded mode work.
type pathView struct {
	degraded bool
	host     runPaths
	user     string
}

func (v pathView) workspace() string {
	if v.degraded {
		return v.host.workspace
	}
	return sandbox.PathWorkspace
}

func (v pathView) home() string {
	if v.degraded {
		return v.host.home
	}
	return sandbox.HomePath(v.user)
}

func (v pathView) jobsDir() string {
	if v.degraded {
		return filepath.Join(v.host.workspace, "jobs")
	}
	return sandbox.PathJobDir
}

func (v pathView) jobRoot() string {
	if v.degraded {
		return v.host.jobDir
	}
	return sandbox.JobRootPath(v.host.jobID)
}

func (v pathView) toolDir(t *tool.Tool) string {
	if v.degraded {
		return t.Dir
	}
	return sandbox.PathTool
}

type runPaths struct {
	workspace    string
	home         string
	jobDir       string // host path of <workspace>/jobs/<jobID>
	jobID        string
	logPath      string
	instancePath string
}

// Run executes one job and returns its instance record.
//
// A non-zero exit from the tool is *data*, not an error: it is recorded on the
// instance and returned with a nil error. Callers distinguish "SRCOS could not
// run this" (error) from "the tool ran and failed" (state == failed).
func (l *Local) Run(ctx context.Context, t *tool.Tool, loaded *job.Loaded) (*Instance, error) {
	if t.Backend != tool.BackendLocal {
		return nil, fmt.Errorf("tool %s declares backend %q; this runner only handles %q", t.ID, t.Backend, tool.BackendLocal)
	}
	if len(t.RequiresStorages) > 0 {
		return nil, fmt.Errorf("tool %s requires storages %v, but the StorageProvider lands in Phase 2", t.ID, t.RequiresStorages)
	}
	if loaded.ID == "" {
		return nil, fmt.Errorf("job has no id (job directory name)")
	}

	paths, err := l.prepare(t, loaded.ID)
	if err != nil {
		return nil, err
	}

	inst := &Instance{
		ID:        fmt.Sprintf("%s-%s-%s", l.User, t.ID, paths.jobID),
		User:      l.User,
		Tool:      t.ID,
		Kind:      string(t.Kind),
		JobName:   loaded.Job.Name,
		State:     StatePending,
		Backend:   string(t.Backend),
		Sandbox:   string(t.Sandbox),
		LogPath:   paths.logPath,
		WorkDir:   paths.jobDir,
		Outputs:   loaded.Job.Outputs,
		Tags:      loaded.Job.Tags,
		StartedAt: time.Now().UTC(),
	}
	fail := func(format string, a ...any) *Instance {
		inst.State = StateFailed
		inst.Error = fmt.Sprintf(format, a...)
		inst.EndedAt = time.Now().UTC()
		_ = SaveInstance(paths.instancePath, inst)
		return inst
	}

	spec, err := l.mountSpec(t, paths)
	if err != nil {
		return fail("mount spec: %v", err), nil
	}
	for _, m := range spec.Mounts() {
		inst.Mounts = append(inst.Mounts, fmt.Sprintf("%s:%s:%s:%s", m.HostPath, m.SandboxPath, m.Mode, m.Origin))
	}

	view := pathView{degraded: t.Sandbox == tool.SandboxNone, host: paths, user: l.User}

	argv, cwd, err := resolveCommand(view, t, loaded)
	if err != nil {
		return fail("%v", err), nil
	}

	inner, err := l.buildInner(t, loaded, spec, view, cwd, argv)
	if err != nil {
		return fail("%v", err), nil
	}

	lim := detectLimiter(job.EffectiveResources(loaded.Job, t))
	inst.Limiter = lim.name
	final := lim.wrap(inner)
	inst.Command = final

	logFile, err := os.Create(paths.logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	var sink io.Writer = logFile
	if l.Stdout != nil {
		sink = io.MultiWriter(logFile, l.Stdout)
	}

	cmd := exec.CommandContext(ctx, final[0], final[1:]...)
	cmd.Stdout = sink
	cmd.Stderr = sink
	cmd.Stdin = nil
	// With bwrap the environment travels as --setenv; degraded it travels as
	// an inner `env -i`. Either way the wrapper (systemd-run/prlimit) keeps
	// the host environment so it can do its job.
	if view.degraded {
		cmd.Dir = view.jobRoot()
	}

	inst.State = StateRunning
	if err := SaveInstance(paths.instancePath, inst); err != nil {
		return nil, err
	}

	runErr := cmd.Run()
	inst.EndedAt = time.Now().UTC()
	inst.Duration = inst.EndedAt.Sub(inst.StartedAt).Round(time.Millisecond).String()

	switch {
	case ctx.Err() != nil:
		inst.State = StateFailed
		inst.Error = "cancelled: " + ctx.Err().Error()
	case runErr == nil:
		inst.State = StateSucceeded
	default:
		inst.State = StateFailed
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			inst.ExitCode = exitErr.ExitCode()
			inst.Error = fmt.Sprintf("tool exited with code %d", inst.ExitCode)
		} else {
			inst.Error = runErr.Error()
		}
	}

	inst.Outputs = l.reportOutputs(spec, loaded.Job.Outputs)
	if err := SaveInstance(paths.instancePath, inst); err != nil {
		return nil, err
	}
	return inst, nil
}

// buildInner materializes the sandbox and returns the argv to execute.
func (l *Local) buildInner(t *tool.Tool, loaded *job.Loaded, spec *sandbox.Spec, view pathView, cwd string, argv []string) ([]string, error) {
	switch t.Sandbox {
	case tool.SandboxBwrap, "":
		bwrapPath, ok, why := sandbox.BwrapProbe()
		if !ok {
			return nil, errors.New(why)
		}
		args := sandbox.BwrapArgv(spec, sandbox.BwrapOptions{
			Cwd:  cwd,
			Env:  view.env(t, loaded.Job),
			Argv: argv,
		})
		return append([]string{bwrapPath}, args...), nil

	case tool.SandboxNone:
		// Degraded mode: no namespace isolation. Jail and MountSpec still
		// constrain SRCOS's own API, but this process can read anything the OS
		// user can, and the declared mounts are NOT materialized. The caller
		// surfaces the degradation (ennote's convention).
		//
		// The env carries host paths here (see pathView), so a tool written
		// against the contract still works — which is why 规范 #2 forbids
		// hard-coded paths.
		//
		// The environment is cleared by an inner `env -i` rather than by
		// replacing the outer command's Env: the resource limiter
		// (systemd-run --user) needs the caller's DBUS_SESSION_BUS_ADDRESS and
		// XDG_RUNTIME_DIR to reach the user manager, so the wrapper must
		// inherit the host environment while the tool must not.
		return append(append([]string{"env", "-i"}, view.env(t, loaded.Job)...), argv...), nil

	case tool.SandboxApptainer:
		return nil, errors.New("sandbox: apptainer is not implemented yet (Phase 6)")

	default:
		return nil, fmt.Errorf("unsupported sandbox %q", t.Sandbox)
	}
}

// resolveCommand decides what to execute and from where.
//
// Two submission styles are supported (ADR-004):
//
//   - the job directory carries its own work.sh — the tool UI generated the
//     script for this particular run;
//   - it does not — the tool package's declared `entry` is used instead.
//
// The job directory is always the working directory, so a submitted script can
// reference sibling files without absolute paths.
func resolveCommand(view pathView, t *tool.Tool, loaded *job.Loaded) (argv []string, cwd string, err error) {
	cwd = view.jobRoot()

	if len(loaded.Job.Command) > 0 {
		return loaded.Job.Command, cwd, nil
	}

	// A work.sh inside the job directory means the submitter generated this
	// run's script; otherwise the tool package's entry is used.
	if _, statErr := os.Stat(filepath.Join(loaded.Dir, "work.sh")); statErr == nil {
		return []string{"bash", "work.sh"}, cwd, nil
	}

	if t.Entry == "" {
		return nil, "", errors.New("no work.sh in the job directory and the tool declares no entry")
	}
	return []string{"bash", view.toolDir(t) + "/" + t.Entry}, cwd, nil
}

func (l *Local) prepare(t *tool.Tool, jobID string) (runPaths, error) {
	p := runPaths{
		workspace: config.WorkspaceDir(l.ConfigDir, l.User, t.ID),
		home:      config.HomeDir(l.ConfigDir, l.User),
		jobDir:    filepath.Join(config.JobsDir(l.ConfigDir, l.User, t.ID), jobID),
		jobID:     jobID,
	}
	p.logPath = filepath.Join(config.LogsDir(l.ConfigDir, l.User, t.ID), jobID+".log")
	p.instancePath = InstancePath(l.ConfigDir, l.User, t.ID, jobID)

	for _, d := range []string{p.workspace, p.home, p.jobDir, filepath.Dir(p.logPath)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return p, err
		}
	}

	if t.Workspace != nil && t.Workspace.InitFrom != "" {
		if err := InitFromTemplate(filepath.Join(t.Dir, t.Workspace.InitFrom), p.workspace); err != nil {
			return p, err
		}
	}
	if t.Home != nil && t.Home.InitFrom != "" {
		if err := InitFromTemplate(filepath.Join(t.Dir, t.Home.InitFrom), p.home); err != nil {
			return p, err
		}
	}
	return p, nil
}

// InitFromTemplate copies a template directory into target exactly once.
//
// The marker lives inside the target: a user who deliberately deletes the
// seeded files should not have them silently restored on the next run.
func InitFromTemplate(templateDir, target string) error {
	info, err := os.Stat(templateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("init_from %q does not exist", templateDir)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("init_from %q is not a directory", templateDir)
	}
	marker := filepath.Join(target, ".srcos-initialized")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := CopyTree(templateDir, target); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte("initialized from "+templateDir+"\n"), 0o644)
}

// CopyTree copies regular files and directories, preserving permission bits.
func CopyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // skip sockets/devices; templates never need them
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, fi.Mode().Perm())
	})
}

// mountSpec builds the sandbox view. Intent order:
//
//	builtin (workspace, virtual home, tool package) -> environment -> data storages
//
// Every entry is an exact path. Binding a parent would hand the sandbox
// everything under it that is group-readable (see package sandbox docs).
func (l *Local) mountSpec(t *tool.Tool, p runPaths) (*sandbox.Spec, error) {
	spec := &sandbox.Spec{}

	builtin := []sandbox.Mount{
		{
			HostPath:    p.workspace,
			SandboxPath: sandbox.PathWorkspace,
			Mode:        sandbox.ReadWrite,
			Origin:      "builtin",
		},
		{
			HostPath:    p.home,
			SandboxPath: sandbox.HomePath(l.User),
			Mode:        sandbox.ReadWrite,
			Origin:      "builtin",
		},
		{
			HostPath:    t.Dir,
			SandboxPath: sandbox.PathTool,
			Mode:        sandbox.ReadOnly,
			Origin:      "tool",
		},
	}
	for _, m := range builtin {
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
				"(%s are bound read-only, so bubblewrap cannot create a mount point there) — "+
				"mount into %s instead, which is first on the sandbox PATH",
				rm.SandboxPath, strings.Join([]string{"/usr", "/bin", "/lib"}, "/"), sandbox.PathToolBin)
		}
		if _, err := os.Stat(rm.Host); err != nil {
			return nil, fmt.Errorf("ro_mounts: host %q is not readable: %w", rm.Host, err)
		}
		if err := spec.Add(sandbox.Mount{
			HostPath:    rm.Host,
			SandboxPath: rm.SandboxPath,
			Mode:        sandbox.ReadOnly,
			Origin:      "env",
		}); err != nil {
			return nil, err
		}
	}

	// Data storages (requires_storages) land here in Phase 2, sourced from a
	// StorageProvider. Run rejected them earlier with a clear message.
	return spec, nil
}

// env renders the complete environment the tool will see (tool-spec §4.1).
//
// All paths come from the pathView, so this is correct in both the sandboxed
// and the degraded case.
func (v pathView) env(t *tool.Tool, j *job.Job) []string {
	home := v.home()
	// In a sandbox PATH is part of the contract (it includes /opt/srcos/bin).
	// Degraded, there is no sandbox to attach to, so the host PATH is carried
	// over — otherwise a tool could not even find `bash`.
	path := sandbox.DefaultPath
	if v.degraded {
		path = os.Getenv("PATH")
	}
	return append([]string{
		"PATH=" + path,
		"SRCOS_WORKSPACE=" + v.workspace(),
		"SRCOS_HOME=" + home,
		// HOME points at the *virtual* home, so .cache/.condarc/.config land in
		// an isolated directory instead of the OS user's real home (ADR-021).
		"HOME=" + home,
		"SRCOS_JOB_DIR=" + v.jobsDir(),
		"SRCOS_JOB_ROOT=" + v.jobRoot(),
		"SRCOS_USER=" + v.user,
		"SRCOS_TOOL=" + t.ID,
		"SRCOS_TASK_ID=" + v.host.jobID,
		"SRCOS_TOOL_VERSION=" + t.Version,
	}, job.ParamEnv(job.EffectiveParams(j, t))...)
}

// reportOutputs resolves declared output paths and annotates which actually
// exist, keeping "declared" and "produced" distinct in the user surface.
func (l *Local) reportOutputs(spec *sandbox.Spec, declared []string) []string {
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
