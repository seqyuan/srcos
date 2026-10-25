package runtime

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/safefs"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// State is the lifecycle state of an instance.
//
// Task and service instances share this set; a service simply lives in Running
// far longer and passes through Stopping on the way out (ADR-003).
type State string

const (
	StatePending   State = "pending"
	StateStarting  State = "starting" // service: process up, healthcheck not yet green
	StateRunning   State = "running"
	StateIdle      State = "idle"      // service: running but no traffic past idleTTL
	StateSubmitted State = "submitted" // task with a doneWhen probe: exit 0 means "submitted"
	StateStopping  State = "stopping"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateStopped   State = "stopped"
)

// Terminal reports whether no further transition is expected.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateStopped:
		return true
	}
	return false
}

// ConsumesResources reports whether an instance is holding the resources it
// asked for.
//
// `pending` deliberately does not count: it is *queued*, and a queue is exactly
// what lets someone submit more work than the host can run at once. What the
// quota has to bound is how much runs simultaneously — otherwise a submission
// could not pass the very quota check its own queue position implies.
func (s State) ConsumesResources() bool {
	switch s {
	case StatePending, StateSucceeded, StateFailed, StateStopped:
		return false
	}
	return true
}

// Instance is the persisted record of one RunUnit.
//
// Task and service instances share this table; only Kind differs (ADR-003).
type Instance struct {
	ID      string `yaml:"id"`
	User    string `yaml:"user"`
	Tool    string `yaml:"tool"`
	Kind    string `yaml:"kind"`
	JobName string `yaml:"job_name"`
	State   State  `yaml:"state"`

	// ExitCode and Error are meaningful for tasks; a service that fails its
	// healthcheck records Error with exit code -1.
	ExitCode int    `yaml:"exit_code"`
	Error    string `yaml:"error,omitempty"`

	// ToolDigest is the content hash of the tool package at the moment this
	// instance was prepared. `version` is a string someone wrote; this is what
	// makes "which code ran" answerable when the files changed without the
	// version changing.
	ToolDigest string `yaml:"tool_digest,omitempty"`

	Backend string   `yaml:"backend"`
	Sandbox string   `yaml:"sandbox"`
	Limiter string   `yaml:"limiter"` // systemd-run | prlimit | none
	Command []string `yaml:"command,omitempty"`
	Mounts  []string `yaml:"mounts,omitempty"` // "host:sandbox:mode:origin"

	// Ingress is the effective route configuration captured when the service
	// started. Routes are rebuilt every scan tick from the instance records, so
	// anything only present in tool.yaml (bwlimit, backend_path, an explicit
	// websocket: false) would otherwise be lost within one tick and again after
	// a gateway restart. Configured distinguishes a record written by a build
	// that knows these fields from a legacy record; the route rebuild may fall
	// back to the tool package for the latter.
	Ingress IngressRoute `yaml:"ingress,omitempty"`

	// Service-only fields.
	Endpoint    string `yaml:"endpoint,omitempty"` // 127.0.0.1:<port>
	RoutePath   string `yaml:"route_path,omitempty"`
	Healthcheck string `yaml:"healthcheck,omitempty"`
	// BackendRef identifies the unit inside the backend: a systemd unit name
	// for local, an SGE job id for sge.
	BackendRef string `yaml:"backend_ref,omitempty"`
	// PID / PIDStart record the child process when SRCOS started one directly,
	// which is the degraded path (no user systemd, ADR-014). PIDStart is the
	// Linux start time in clock ticks: a pid alone is a number the OS reuses,
	// so stopping a recorded pid without checking it could kill an unrelated
	// process. Both are zero when the backend owns the process (systemd unit,
	// SGE job).
	PID      int    `yaml:"pid,omitempty"`
	PIDStart uint64 `yaml:"pid_start,omitempty"`

	LogPath string `yaml:"log_path"`

	// For tasks, WorkDir is the job directory. For services there is no job
	// directory, so it is the workspace.
	WorkDir string `yaml:"work_dir"`

	StartedAt time.Time `yaml:"started_at"`
	EndedAt   time.Time `yaml:"ended_at,omitempty"`
	Duration  string    `yaml:"duration,omitempty"`
	// LastActiveAt drives idle reaping; the proxy touches it on every request.
	LastActiveAt time.Time `yaml:"last_active_at,omitempty"`
	// LeaseExpiresAt is when the scheduler's own ceiling on this unit runs out
	// (SGE's h_rt). A renewal pushes it out so a long-running service is not
	// killed while the platform still wants it. Zero when no such ceiling
	// applies (local units, SGE jobs without a declared walltime).
	LeaseExpiresAt time.Time `yaml:"lease_expires_at,omitempty"`
	// LeaseWarnedDeadline is the LeaseExpiresAt value already warned about, so a
	// scan tick does not repeat the same warning; a renewal changes the deadline
	// and re-arms it.
	LeaseWarnedDeadline time.Time `yaml:"lease_warned_deadline,omitempty"`

	// UnitName is the backend unit name derived from the instance id. It is
	// persisted before the unit starts, so a restart can find (or rule out) a
	// process whose record was never advanced to `running`.
	UnitName string `yaml:"unit_name,omitempty"`

	// DoneWhen is the effective completion probe for a task (job overrides the
	// tool). DoneWhenHost is the resolved host path, stored so a restart does not
	// have to rebuild the mount table, and DoneWhenDeadline is when an
	// unsatisfied probe becomes a failure.
	//
	// They are persisted because a doneWhen probe outlives the process: after a
	// restart the platform must be able to continue waiting instead of treating
	// exit 0 as success.
	DoneWhen         *tool.DoneWhen `yaml:"done_when,omitempty"`
	DoneWhenHost     string         `yaml:"done_when_host,omitempty"`
	DoneWhenDeadline time.Time      `yaml:"done_when_deadline,omitempty"`

	Outputs []string          `yaml:"outputs,omitempty"`
	Tags    map[string]string `yaml:"tags,omitempty"`

	// RequestedCPU / RequestedMemory record what this instance actually asked
	// for, so an aggregate quota sums real usage instead of guessing from the
	// tool's ceiling.
	RequestedCPU    int    `yaml:"requested_cpu,omitempty"`
	RequestedMemory string `yaml:"requested_memory,omitempty"`
}

// CPURequest is the instance's CPU request in cores.
func (i *Instance) CPURequest() int { return i.RequestedCPU }

// MemoryRequestBytes is the instance's memory request in bytes (0 if unknown).
//
// An unparsable value yields 0, which under-counts usage. That is the unsafe
// direction for a quota, so the field is written only by requestedResources,
// which copies a value ParseMemory already accepted.
func (i *Instance) MemoryRequestBytes() uint64 {
	if i.RequestedMemory == "" {
		return 0
	}
	b, err := tool.ParseMemory(i.RequestedMemory)
	if err != nil {
		return 0
	}
	return b
}

// InstanceID is the deterministic id for (user, tool) plus a discriminator.
//
// A task's discriminator is its job id, so a task is addressable by job. A
// service's discriminator is "svc", because there is exactly one live service
// instance per (user, tool).
func InstanceID(user, tool, discriminator string) string {
	if discriminator == "" {
		discriminator = "svc"
	}
	return fmt.Sprintf("%s-%s-%s", user, tool, discriminator)
}

// InstancePath is the record location for an instance id.
func InstancePath(configDir, id string) string {
	return filepath.Join(config.InstancesDir(configDir), id+".yaml")
}

// SaveInstance writes the record atomically so a crash mid-write cannot leave
// a half-parsed YAML behind.
//
// The temp file is unique per writer, not "<path>.tmp": the gateway, a CLI
// `svc stop` and the admin console may all save the same instance, and a fixed
// temp name lets one process rename another's half-written content into place.
// The file and directory are synced because this record is the authority on
// what is running; losing it after a crash would turn a live unit into an
// orphan with no way to reconcile it.
func SaveInstance(path string, inst *Instance) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(inst)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	// Best effort: the rename is already atomic; syncing the directory makes
	// the new name durable across a power loss too.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
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

// DeleteInstance removes a record.
func DeleteInstance(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ListInstances returns every record under data/instances, newest first.
//
// A record that cannot be parsed is skipped rather than failing the whole
// listing — one corrupt file must not blind the user to every other instance —
// but it is logged once per process (the scan tick calls this several times a
// minute, and a permanently corrupt record must not fill the log).
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
		path := filepath.Join(config.InstancesDir(configDir), e.Name())
		inst, err := LoadInstance(path)
		if err != nil {
			warnUnreadableInstance(path, err)
			continue
		}
		out = append(out, inst)
	}
	sortNewestFirst(out)
	return out, nil
}

// warnedInstances dedupes the "unreadable record" log line per path, so a
// corrupt file is reported once rather than on every scan tick.
//
// The map grows without bound in principle, but only a path whose record is
// corrupt ever enters it, and entries are tiny — bounded by the number of bad
// files an operator has, which is the point of the warning. A TTL would be
// complexity for a problem that does not exist.
var warnedInstances sync.Map // path -> struct{}

func warnUnreadableInstance(path string, err error) {
	if _, seen := warnedInstances.LoadOrStore(path, struct{}{}); seen {
		return
	}
	log.Printf("[srcos] instance record %s is unreadable and was skipped: %v", filepath.Base(path), err)
}

// sortNewestFirst orders records newest-first, with the id as a stable
// tiebreaker. sort.Slice, not an insertion sort: the record set only grows, and
// this runs on every scan tick and on every listing.
func sortNewestFirst(list []*Instance) {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.StartedAt.Equal(b.StartedAt) {
			return a.ID < b.ID
		}
		return a.StartedAt.After(b.StartedAt)
	})
}

// IngressRoute is the persisted copy of a service's ingress settings.
//
// It lives on the instance record because the record is the source of truth
// the scan tick, the proxy and a restarted gateway all rebuild routes from.
// Keeping it out of tool.yaml at this point is deliberate: the tool package may
// be upgraded (or removed) while an instance is running, and the instance must
// keep the settings it actually started with.
type IngressRoute struct {
	WebSocket   *bool  `yaml:"websocket,omitempty"`
	BWLimit     int64  `yaml:"bwlimit,omitempty"`
	BackendPath string `yaml:"backend_path,omitempty"`
	Configured  bool   `yaml:"configured,omitempty"`
}

// IngressForTool renders the effective settings of a tool's ingress.
func IngressForTool(t *tool.Tool) IngressRoute {
	ws := true
	var bw int64
	var bp string
	if t != nil && t.Ingress != nil {
		ws = t.Ingress.WebSocketEnabled()
		bw = t.Ingress.BWLimit
		bp = t.Ingress.BackendPath
	}
	return IngressRoute{WebSocket: &ws, BWLimit: bw, BackendPath: bp, Configured: true}
}

// Paths holds the concrete host paths of one unit.
type Paths struct {
	User string
	Tool string
	// DataDir is the trusted root above every tool-writable directory
	// (workspace, home, flows, logs). Write helpers use it to refuse symlinks
	// below it; it may itself be reached through a symlink.
	DataDir   string
	Workspace string
	Home      string
	// FlowRuns is the user's flow-run directory (data/flows/<user>), mounted at
	// /flow for every unit. It is builtin runtime state — like the workspace and
	// the virtual home, and unlike a data storage (ADR-020) — because it is what
	// makes a flow's edges resolvable: two tools can only share a path if the
	// platform put it somewhere both can see.
	FlowRuns string
	// JobDir is empty for a service: services have no submission directory.
	JobDir     string
	JobID      string
	LogPath    string
	RecordPath string
}

// PathsFor builds the host paths for a task (jobID != "") or a service.
func PathsFor(configDir, user, toolID, jobID string) Paths {
	p := Paths{
		User:      user,
		Tool:      toolID,
		DataDir:   config.DataDir(configDir),
		Workspace: config.WorkspaceDir(configDir, user, toolID),
		Home:      config.HomeDir(configDir, user),
		FlowRuns:  config.FlowRunsDir(configDir, user),
		JobID:     jobID,
	}
	if jobID != "" {
		p.JobDir = filepath.Join(config.JobsDir(configDir, user, toolID), jobID)
		p.LogPath = filepath.Join(config.LogsDir(configDir, user, toolID), jobID+".log")
		p.RecordPath = InstancePath(configDir, InstanceID(user, toolID, jobID))
	} else {
		p.LogPath = filepath.Join(config.LogsDir(configDir, user, toolID), "service.log")
		p.RecordPath = InstancePath(configDir, InstanceID(user, toolID, ""))
	}
	return p
}

// EnsureDirs creates the workspace, virtual home and log directory, applying
// the tool's templates exactly once.
func (p Paths) EnsureDirs(t *tool.Tool) error {
	// FlowRuns is mounted into every sandbox, so it has to exist before bwrap
	// is asked to bind it (an empty directory is the normal state: flows are
	// opt-in, and most tools never look at /flow).
	dirs := []string{p.Workspace, p.Home, p.FlowRuns, filepath.Dir(p.LogPath)}
	if p.JobDir != "" {
		dirs = append(dirs, p.JobDir)
	}
	mkdir := func(d string) error {
		if p.DataDir == "" {
			return os.MkdirAll(d, 0o755)
		}
		return safefs.MkdirAllBeneath(p.DataDir, d, 0o755)
	}
	for _, d := range dirs {
		if err := mkdir(d); err != nil {
			return err
		}
	}
	if t.Workspace != nil && t.Workspace.InitFrom != "" {
		if err := initTemplate(p.DataDir, filepath.Join(t.Dir, t.Workspace.InitFrom), p.Workspace); err != nil {
			return err
		}
	}
	if t.Home != nil && t.Home.InitFrom != "" {
		if err := initTemplate(p.DataDir, filepath.Join(t.Dir, t.Home.InitFrom), p.Home); err != nil {
			return err
		}
	}
	return nil
}

// PathView resolves the contract's sandbox paths into whatever the running
// process will actually see.
//
// With a mount namespace the contract's paths (/workspace, /home/<user>,
// /tool) are literally true. With `sandbox: none` there is no namespace, so
// those paths do not exist on the host and the same variables must carry real
// host paths instead. Tools never need to care: they are required to use
// $SRCOS_* rather than literal paths (tool-spec §4 规范 #2), which is exactly
// what makes the degraded mode work.
type PathView struct {
	Degraded bool
	Paths    Paths
	User     string
	// APIBase is where SRCOS's own REST API is reachable from inside the unit
	// (SRCOS_API). Empty means "no gateway is known from here".
	APIBase string
	// EnvBase is the named environment's variables. They sit between the
	// platform defaults and the tool's own `env:` — so a tool can override an
	// environment, and an environment can override the platform.
	EnvBase []string
}

// NewPathView builds the view for a sandbox mode. apiBase and envBase may be
// empty.
func NewPathView(mode tool.Sandbox, p Paths, apiBase string, envBase []string) PathView {
	return PathView{Degraded: mode == tool.SandboxNone, Paths: p, User: p.User, APIBase: apiBase, EnvBase: envBase}
}

func (v PathView) Workspace() string {
	if v.Degraded {
		return v.Paths.Workspace
	}
	return sandbox.PathWorkspace
}

func (v PathView) Home() string {
	if v.Degraded {
		return v.Paths.Home
	}
	return sandbox.HomePath(v.User)
}

func (v PathView) JobsDir() string {
	if v.Degraded {
		return filepath.Join(v.Paths.Workspace, "jobs")
	}
	return sandbox.PathJobDir
}

func (v PathView) JobRoot() string {
	if v.Degraded {
		return v.Paths.JobDir
	}
	return sandbox.JobRootPath(v.Paths.JobID)
}

// FlowRuns is the host path behind /flow (for the degraded mode, where the
// sandbox path does not exist on the host).
func (v PathView) FlowRuns() string {
	if v.Degraded {
		return v.Paths.FlowRuns
	}
	return sandbox.PathFlow
}

func (v PathView) ToolDir(t *tool.Tool) string {
	if v.Degraded {
		return t.PackageRootDir()
	}
	return sandbox.PathTool
}

// Cwd is the working directory: the job root for a task, the workspace for a
// service.
func (v PathView) Cwd() string {
	if v.Paths.JobDir != "" {
		return v.JobRoot()
	}
	return v.Workspace()
}

// Env renders the complete environment the tool will see (tool-spec §4.1).
func (v PathView) Env(t *tool.Tool, j *job.Job) []string {
	home := v.Home()
	out := []string{
		"PATH=" + v.pathValue(),
		"SRCOS_WORKSPACE=" + v.Workspace(),
		"SRCOS_HOME=" + home,
		// HOME points at the *virtual* home, so .cache/.condarc/.config land in
		// an isolated directory instead of the OS user's real home (ADR-021).
		"HOME=" + home,
		"SRCOS_JOB_DIR=" + v.JobsDir(),
		"SRCOS_USER=" + v.User,
		"SRCOS_TOOL=" + t.ID,
		"SRCOS_TOOL_VERSION=" + t.Version,
		// SRCOS_TOOL_DIR is the package root mounted at /tool. Components that
		// share a package (ui/ and task/) use it instead of cwd or `..`.
		"SRCOS_TOOL_DIR=" + v.ToolDir(t),
	}
	if v.Paths.JobID != "" {
		out = append(out,
			"SRCOS_JOB_ROOT="+v.JobRoot(),
			"SRCOS_TASK_ID="+v.Paths.JobID,
		)
	} else {
		// A service gets an instance identity instead of a task identity.
		out = append(out, "SRCOS_INSTANCE_ID="+InstanceID(v.User, t.ID, ""))
	}
	if j != nil {
		out = append(out, job.ParamEnv(job.EffectiveParams(j, t))...)
	}
	// SRCOS_API: the gateway's REST base, so a tool UI (or an agent hosted by a
	// service) can talk back. Absent when no gateway is reachable from here, and
	// absent for a network-isolated task: its fresh netns makes the host's
	// loopback unreachable, so advertising a URL that can never work is worse
	// than omitting it.
	if v.APIBase != "" && !(t.Kind == tool.KindTask && t.Network == tool.NetworkNone) {
		out = append(out, "SRCOS_API="+v.APIBase)
	}
	// The named environment's variables, then the tool's own: a tool may
	// override an environment, an environment the platform defaults.
	out = append(out, v.EnvBase...)
	// The tool's declared environment goes last so it wins over the defaults
	// above (a tool that sets PATH means it — e.g. R living in a miniforge
	// prefix). Reserved keys are refused at registration.
	out = append(out, t.Env...)
	return out
}

func (v PathView) pathValue() string {
	// In a sandbox PATH is part of the contract (it includes /opt/srcos/bin).
	// Degraded, there is no sandbox to attach to, so the host PATH is carried
	// over — otherwise a tool could not even find `bash`.
	if v.Degraded {
		return os.Getenv("PATH")
	}
	return sandbox.DefaultPath
}

// ErrInstanceChanged tells UpdateInstance not to save: the record is no longer
// in a state the caller may modify (typically a deliberate stop won the race).
var ErrInstanceChanged = errors.New("instance changed since the caller last looked")

// UpdateInstance is the serialized read-modify-write primitive for instance
// records.
//
// It takes a per-record flock (a sibling .lock file, because rename replaces
// the record's inode), reloads the latest record, lets fn mutate it, and saves
// it atomically. If fn returns ErrInstanceChanged the record is left untouched
// and applied=false is returned with the current value. A missing record is
// passed to fn as an empty Instance (the first start creates it).
//
// Callers must not call UpdateInstance recursively for the same path: flock is
// held on a descriptor and a second open would block.
func UpdateInstance(path string, fn func(*Instance) error) (*Instance, bool, error) {
	lock, err := lockInstance(path)
	if err != nil {
		return nil, false, err
	}
	defer lock.release()

	cur, err := LoadInstance(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, false, err
		}
		cur = &Instance{}
	}
	before := *cur
	if err := fn(cur); err != nil {
		if errors.Is(err, ErrInstanceChanged) {
			// Return what is on disk, not the caller's half-applied mutation:
			// callers use this value as the winning record.
			return &before, false, nil
		}
		return nil, false, err
	}
	if err := SaveInstance(path, cur); err != nil {
		return nil, false, err
	}
	return cur, true, nil
}
