package runtime

import (
	"fmt"
	"os"
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

	Backend string   `yaml:"backend"`
	Sandbox string   `yaml:"sandbox"`
	Limiter string   `yaml:"limiter"` // systemd-run | prlimit | none
	Command []string `yaml:"command,omitempty"`
	Mounts  []string `yaml:"mounts,omitempty"` // "host:sandbox:mode:origin"

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

// DeleteInstance removes a record.
func DeleteInstance(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
// 路径
// ─────────────────────────────────────────────────────────────────────────

// Paths holds the concrete host paths of one unit.
type Paths struct {
	User      string
	Tool      string
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
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if t.Workspace != nil && t.Workspace.InitFrom != "" {
		if err := InitFromTemplate(filepath.Join(t.Dir, t.Workspace.InitFrom), p.Workspace); err != nil {
			return err
		}
	}
	if t.Home != nil && t.Home.InitFrom != "" {
		if err := InitFromTemplate(filepath.Join(t.Dir, t.Home.InitFrom), p.Home); err != nil {
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
}

// NewPathView builds the view for a sandbox mode.
func NewPathView(mode tool.Sandbox, p Paths) PathView {
	return PathView{Degraded: mode == tool.SandboxNone, Paths: p, User: p.User}
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
		return t.Dir
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
