// Package tool defines the SRCOS tool contract: the in-memory shape of
// tool.yaml plus its validation rules.
//
// The contract itself is documented in docs/tool-spec.md; this package is the
// executable form of it. Validation errors are collected rather than returned
// one at a time, because a tool author fixing a manifest wants the whole list.
package tool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind distinguishes the two shapes of RunUnit (see ADR-003).
type Kind string

const (
	KindTask    Kind = "task"
	KindService Kind = "service"
)

// Backend is where the instance actually runs (ADR-002).
type Backend string

const (
	BackendLocal Backend = "local"
	BackendSGE   Backend = "sge"
)

// Sandbox is the isolation mechanism; it is an attribute of a backend, not a
// backend of its own (ADR-002/ADR-014).
type Sandbox string

const (
	SandboxNone      Sandbox = "none"
	SandboxBwrap     Sandbox = "bwrap"
	SandboxApptainer Sandbox = "apptainer"
)

// InputType is the frozen type set of interface.inputs (ADR-018: this set is
// frozen in Phase 1, additions only).
type InputType string

const (
	TypeString    InputType = "string"
	TypeInt       InputType = "int"
	TypeFloat     InputType = "float"
	TypeBool      InputType = "bool"
	TypeEnum      InputType = "enum"
	TypeFile      InputType = "file"
	TypeDirectory InputType = "directory"
	TypeDirPath   InputType = "dirpath" // annopi-compatible alias of directory
	TypePath      InputType = "path"    // storage-bound path (ADR-020)
)

// OutputType is deliberately narrower than InputType: outputs are only ever
// files or directories.
type OutputType string

const (
	OutFile      OutputType = "file"
	OutDirectory OutputType = "directory"
)

// Tool is the parsed tool.yaml.
type Tool struct {
	SchemaVersion int    `yaml:"schemaVersion"`
	ID            string `yaml:"id"`
	Version       string `yaml:"version"`
	Name          string `yaml:"name"`
	Description   string `yaml:"description,omitempty"`

	Kind    Kind     `yaml:"kind"`
	Backend Backend  `yaml:"backend"`
	Sandbox Sandbox  `yaml:"sandbox,omitempty"`
	Image   string   `yaml:"image,omitempty"`
	Env     []string `yaml:"env,omitempty"`

	ROMounts []ROMount `yaml:"ro_mounts,omitempty"`
	Entry    string    `yaml:"entry"`
	// Command is a declarative argv, as an alternative to an entry script:
	// the common "start this app server" case without a work.sh that only
	// re-derives the port and the interpreter path. argv is exec'd directly (no
	// shell), so there is no quoting or injection surface. `${NAME}` references
	// are expanded from the unit's own environment — the values are literally
	// the ones SRCOS injects, so a command and its environment cannot disagree.
	//
	// Exactly one of Command / Entry must be set.
	Command []string `yaml:"command,omitempty"`

	Interface        Interface `yaml:"interface"`
	RequiresStorages []string  `yaml:"requires_storages,omitempty"`
	Resources        Resources `yaml:"resources"`
	Internal         *Internal `yaml:"internal,omitempty"`

	Ingress    *Ingress      `yaml:"ingress,omitempty"`
	Lifecycle  *Lifecycle    `yaml:"lifecycle,omitempty"`
	Completion *Completion   `yaml:"completion,omitempty"`
	Workspace  *InitTemplate `yaml:"workspace,omitempty"`
	Home       *InitTemplate `yaml:"home,omitempty"`

	// Agent declares that this service hosts an agent that calls SRCOS's MCP
	// endpoint (A1). When set, an agent token is minted at instance start and
	// revoked when it ends.
	Agent *AgentSpec `yaml:"agent,omitempty"`

	// Environment is the id of a named environment declared in
	// config/environments.yaml: where the interpreter and its libraries live on
	// *this* host. Naming it instead of writing host paths keeps the tool
	// package portable.
	Environment string `yaml:"environment,omitempty"`

	// Dir is the tool package directory this manifest was loaded from. It is
	// not part of tool.yaml.
	Dir string `yaml:"-"`
}

// AgentSpec declares what an agent hosted by this service needs from SRCOS
// (A1). It exists so a hosted agent does not have to be handed a user-level
// credential: SRCOS mints one whose scopes and allowlist come from *here*, so it
// is a subset of its owner's permissions by construction, hands it to the
// sandbox as a file in the virtual home, and revokes it when the instance ends.
type AgentSpec struct {
	// MCP is the scope set the minted token carries: read and/or submit.
	MCP []string `yaml:"mcp"`
	// Tools narrows submit to these tool ids (empty = every tool the owner may
	// use). It can only narrow — the owner's Grant is still the outer bound, and
	// the usual tool × user checks run at submit time.
	Tools []string `yaml:"tools,omitempty"`
}

// ROMount mounts a host path read-only into the sandbox. This is for
// *environment* (conda, module, .sif), never for data — data goes through
// storages (see AGENTS.md: 环境与数据要分开).
type ROMount struct {
	Host        string `yaml:"host"`
	SandboxPath string `yaml:"sandbox_path"`
}

// Interface is the machine-readable signature. Its json tags mirror the yaml
// ones because the same shape is served over HTTP and (in Phase 3.5) exposed as
// an MCP schema — three consumers, one spelling (ADR-018).
type Interface struct {
	Inputs  []Input  `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs []Output `yaml:"outputs,omitempty" json:"outputs,omitempty"`
}

type Input struct {
	Name        string    `yaml:"name" json:"name"`
	Type        InputType `yaml:"type" json:"type"`
	Label       string    `yaml:"label,omitempty" json:"label,omitempty"`
	Description string    `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool      `yaml:"required,omitempty" json:"required,omitempty"`
	Default     any       `yaml:"default,omitempty" json:"default,omitempty"`

	// type: enum
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
	// type: int|float
	Min *float64 `yaml:"min,omitempty" json:"min,omitempty"`
	Max *float64 `yaml:"max,omitempty" json:"max,omitempty"`
	// type: path (required) — comma-separated storage ids
	From string `yaml:"from,omitempty" json:"from,omitempty"`
	// type: path — file | directory
	Select string `yaml:"select,omitempty" json:"select,omitempty"`
	// type: file|directory|path — required file names to validate
	Files []string `yaml:"files,omitempty" json:"files,omitempty"`
}

type Output struct {
	Name        string     `yaml:"name" json:"name"`
	Type        OutputType `yaml:"type" json:"type"`
	Label       string     `yaml:"label,omitempty" json:"label,omitempty"`
	Description string     `yaml:"description,omitempty" json:"description,omitempty"`
	Provides    []string   `yaml:"provides,omitempty" json:"provides,omitempty"`
}

type Resources struct {
	CPU int `yaml:"cpu" json:"cpu"`
	// Memory is a human-readable size ("32Gi", "512M"); ParseMemory is the only
	// reader, so the unit grammar is defined in one place.
	Memory   string `yaml:"memory" json:"memory"`
	Walltime string `yaml:"walltime,omitempty" json:"walltime,omitempty"`
	Queue    string `yaml:"queue,omitempty" json:"queue,omitempty"`
	GPU      int    `yaml:"gpu,omitempty" json:"gpu,omitempty"`
	// HMem and VMem are SGE-specific aliases kept for annopi compatibility.
	HMem string `yaml:"h_vmem,omitempty" json:"h_vmem,omitempty"`
	VMem string `yaml:"v_mem,omitempty" json:"v_mem,omitempty"`
}

// Internal is advisory: SRCOS validates the combination but never executes
// the tool's internal parallelism itself (ADR-005).
type Internal struct {
	Executor    string `yaml:"executor"` // local | qsubsge
	Parallelism int    `yaml:"parallelism,omitempty"`
}

type Ingress struct {
	// Port is the port the app would like to bind *inside its unit*. It is
	// optional, and only a preference: on `local` the platform assigns
	// SRCOS_PORT and the app must listen on that; on `sge` two service jobs can
	// land on one node, so the job prefers this port and falls back to a free
	// one, publishing whichever it got. Local-only tools should simply omit it.
	Port        int          `yaml:"port,omitempty"`
	Healthcheck *Healthcheck `yaml:"healthcheck,omitempty"`
	// BackendPath is the prefix the backend itself expects (the same meaning as
	// the static service card option). Empty means "the app is served at its
	// root", which is the normal case for a service instance: SRCOS strips its
	// own /proxy/<user>/<tool> prefix before forwarding.
	//
	// It is deliberately *not* the healthcheck path: the probe path says where to
	// knock, not where the app lives.
	BackendPath string `yaml:"backend_path,omitempty"`
	// WebSocket allows protocol upgrades through the proxy. Nil means enabled:
	// an app server usually needs it and the upgrade path is harmless when
	// nothing asks for one.
	WebSocket *bool `yaml:"websocket,omitempty"`
	// BWLimit caps this instance's bandwidth in bytes/sec (0 = unlimited). It
	// is enforced by the shared proxy path, so an instance and a static card
	// behave the same.
	BWLimit int64 `yaml:"bwlimit,omitempty"`
}

// WebSocketEnabled reports whether the proxy should allow protocol upgrades.
func (i *Ingress) WebSocketEnabled() bool {
	if i == nil || i.WebSocket == nil {
		return true
	}
	return *i.WebSocket
}

// HealthcheckPath is the probe path, defaulting to the root.
func (i *Ingress) HealthcheckPath() string {
	if i == nil || i.Healthcheck == nil || i.Healthcheck.Path == "" {
		return "/"
	}
	return i.Healthcheck.Path
}

type Healthcheck struct {
	Path         string `yaml:"path,omitempty"`
	Timeout      string `yaml:"timeout,omitempty"`
	StartupGrace string `yaml:"startup_grace,omitempty"`
}

type Lifecycle struct {
	Restart     string `yaml:"restart,omitempty"`
	MaxLifetime string `yaml:"max_lifetime,omitempty"`
	IdleTTL     string `yaml:"idle_ttl,omitempty"`
}

// Completion is the doneWhen escape hatch (ADR-006).
//
// The hatching case where a tool cannot block (e.g. it submits child jobs and
// returns) declares a probe instead: exit code 0 then only means "submission
// succeeded", and SRCOS waits for the probe before marking success.
type Completion struct {
	DoneWhen        *DoneWhen `yaml:"doneWhen,omitempty"`
	DoneWhenTimeout string    `yaml:"doneWhenTimeout,omitempty"`
}

// DoneWhenType enumerates the built-in probe kinds (tool-spec §7.3).
type DoneWhenType string

const (
	DoneWhenFileExists  DoneWhenType = "file_exists"
	DoneWhenDirNonEmpty DoneWhenType = "dir_nonempty"
	DoneWhenExitCode    DoneWhenType = "exit_code"
)

type DoneWhen struct {
	Type DoneWhenType `yaml:"type"`
	Path string       `yaml:"path,omitempty"`
}

type InitTemplate struct {
	InitFrom string `yaml:"init_from,omitempty"`
}

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	nameRe = regexp.MustCompile(`^[a-z0-9_][a-z0-9_-]*$`)
)

// CheckValue validates one value against this input's declared type and range.
//
// It lives here rather than in package job so that both a tool-level `default`
// and a job-supplied value are checked by the same code: registration must
// reject a default that contradicts its own declaration.
//
// Numbers arrive as json.Number when they come from job.json, so the numeric
// cases accept that spelling too.
func (in Input) CheckValue(v any) error {
	switch in.Type {
	case TypeString, TypeFile, TypeDirectory, TypeDirPath, TypePath:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("want a string for type %s, got %T", in.Type, v)
		}
	case TypeBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("want a boolean, got %T", v)
		}
	case TypeEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want a string, got %T", v)
		}
		for _, allowed := range in.Values {
			if s == allowed {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %v", s, in.Values)
	case TypeInt:
		n, err := numeric(v)
		if err != nil {
			return err
		}
		if n != float64(int64(n)) {
			return fmt.Errorf("want an integer, got %v", n)
		}
	case TypeFloat:
		if _, err := numeric(v); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown type %q", in.Type)
	}

	if in.Type == TypeInt || in.Type == TypeFloat {
		n, _ := numeric(v)
		if in.Min != nil && n < *in.Min {
			return fmt.Errorf("%v is below min %v", n, *in.Min)
		}
		if in.Max != nil && n > *in.Max {
			return fmt.Errorf("%v is above max %v", n, *in.Max)
		}
	}
	return nil
}

func numeric(v any) (float64, error) {
	switch x := v.(type) {
	case json.Number:
		return x.Float64()
	case float64:
		return x, nil
	case float32:
		return float64(x), nil
	case int:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(x), 64)
	default:
		return 0, fmt.Errorf("want a number, got %T", v)
	}
}

// Load reads and validates a tool.yaml from dir.
func Load(dir string) (*Tool, error) {
	path := filepath.Join(dir, "tool.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Tool
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t.Dir = dir
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &t, nil
}

// Discover scans a tools directory for <id>/tool.yaml packages.
func Discover(root string) ([]*Tool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var tools []*Tool
	seen := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "tool.yaml")); err != nil {
			continue
		}
		t, err := Load(dir)
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[t.ID]; dup {
			return nil, fmt.Errorf("duplicate tool id %q: %s and %s", t.ID, prev, dir)
		}
		seen[t.ID] = dir
		tools = append(tools, t)
	}
	return tools, nil
}

// Find returns the tool with the given id under root.
func Find(root, id string) (*Tool, error) {
	return Load(filepath.Join(root, id))
}

// FindIn is Find over an already-discovered set (avoids re-reading the disk).
func FindIn(tools []*Tool, id string) *Tool {
	for _, t := range tools {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// Validate applies every registration-time rule from tool-spec §9.
func (t *Tool) Validate() error {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	if t.SchemaVersion != 1 {
		bad("schemaVersion must be 1, got %d", t.SchemaVersion)
	}
	if !idRe.MatchString(t.ID) {
		bad("id %q must match %s", t.ID, idRe)
	}
	if t.Version == "" {
		bad("version is required")
	}
	if t.Name == "" {
		bad("name is required")
	}
	if _, err := ParseWalltimeOrZero(t.Resources.Walltime); err != nil {
		bad("resources.walltime: %v", err)
	}
	if _, err := ParseMemory(t.Resources.Memory); err != nil {
		bad("resources.memory: %v", err)
	}
	if t.Resources.CPU < 1 {
		bad("resources.cpu must be >= 1, got %d", t.Resources.CPU)
	}

	// ── kind ────────────────────────────────────────────────────────────
	switch t.Kind {
	case KindTask:
		if t.Ingress != nil {
			bad("kind: task must not declare ingress (tasks have no port)")
		}
		if t.Lifecycle != nil {
			bad("kind: task must not declare lifecycle (use resources.walltime)")
		}
		if t.Resources.Walltime == "" {
			bad("kind: task requires resources.walltime")
		}
	case KindService:
		if t.Ingress == nil {
			bad("kind: service requires ingress")
		}
		if t.Lifecycle == nil {
			bad("kind: service requires lifecycle")
		}
	default:
		bad("kind must be %q or %q, got %q", KindTask, KindService, t.Kind)
	}

	// ── backend / sandbox ───────────────────────────────────────────────
	switch t.Backend {
	case BackendLocal, BackendSGE:
	default:
		bad("backend must be %q or %q, got %q", BackendLocal, BackendSGE, t.Backend)
	}
	switch t.Sandbox {
	case "":
		t.Sandbox = SandboxNone
	case SandboxNone, SandboxBwrap, SandboxApptainer:
	default:
		bad("sandbox must be one of none|bwrap|apptainer, got %q", t.Sandbox)
	}
	if t.Sandbox == SandboxApptainer && t.Image == "" {
		bad("sandbox: apptainer requires image")
	}
	if t.Ingress != nil && t.Ingress.BackendPath != "" && !pathIsAbs(t.Ingress.BackendPath) {
		bad("ingress.backend_path must be absolute, got %q", t.Ingress.BackendPath)
	}
	if t.Ingress != nil && t.Ingress.Port < 0 {
		bad("ingress.port must be >= 0 (it is a preference for sge; local assigns SRCOS_PORT), got %d", t.Ingress.Port)
	}
	if t.Ingress != nil && t.Ingress.BWLimit < 0 {
		bad("ingress.bwlimit must be >= 0 bytes/sec (0 = unlimited), got %d", t.Ingress.BWLimit)
	}
	if t.Sandbox != SandboxApptainer && t.Image != "" {
		bad("image is only meaningful with sandbox: apptainer (got sandbox: %s)", t.Sandbox)
	}

	// ── agent (A1) ────────────────────────────────────────
	// Only a service can host an agent: the credential's lifetime is the
	// instance's, and only a long-running unit has one.
	if t.Agent != nil {
		if t.Kind != KindService {
			bad("agent is only meaningful for kind: service (a hosted agent is a long-running unit)")
		}
		submit := false
		for _, sc := range t.Agent.MCP {
			switch strings.TrimSpace(sc) {
			case "read":
			case "submit":
				submit = true
			default:
				bad("agent.mcp must be read|submit, got %q", sc)
			}
		}
		if len(t.Agent.MCP) == 0 {
			bad("agent.mcp must name at least one scope (read|submit)")
		}
		if len(t.Agent.Tools) > 0 && !submit {
			bad("agent.tools narrows submit, so agent.mcp must include submit")
		}
	}

	// ── executor × backend cross-check (ADR-007) ────────────────────────
	if t.Internal != nil {
		switch t.Internal.Executor {
		case "local", "qsubsge", "":
		default:
			bad("internal.executor must be local|qsubsge, got %q", t.Internal.Executor)
		}
		if t.Internal.Executor == "qsubsge" && t.Backend == BackendSGE {
			bad("internal.executor: qsubsge with backend: sge is illegal — " +
				"most SGE sites forbid nested qsub, so a submitter must run on the login node (backend: local)")
		}
	}

	// ── environment ──────────────────────────────────────
	// Only the shape is checkable here; "is it declared in
	// config/environments.yaml" needs the deployment, and is answered where a
	// provider is available (tool validate, and the unit's prepare).
	if t.Environment != "" && !idRe.MatchString(t.Environment) {
		bad("environment %q must match %s (an id declared in config/environments.yaml)", t.Environment, idRe)
	}

	// ── entry / command ─────────────────────────────────────
	// Exactly one: either a declarative argv (the common case for an app
	// server) or a script (anything with loops, conditionals or several
	// processes).
	switch {
	case t.Entry != "" && len(t.Command) > 0:
		bad("entry and command are mutually exclusive (entry is the script escape hatch, command the declarative one)")
	case t.Entry == "" && len(t.Command) == 0:
		bad("one of entry or command is required")
	case len(t.Command) > 0:
		validateCommand(t.Command, t.Kind, bad)
	}
	if t.Entry != "" && t.Dir != "" {
		if _, err := os.Stat(filepath.Join(t.Dir, t.Entry)); err != nil {
			bad("entry %q not found in tool directory", t.Entry)
		}
	}

	// ── storages ────────────────────────────────────────────────────────
	declared := map[string]bool{}
	for _, s := range t.RequiresStorages {
		if s == "" {
			bad("requires_storages contains an empty id")
			continue
		}
		declared[s] = true
	}

	// A declared storage is materialized as a bind mount, so it only exists
	// inside a mount namespace. With sandbox: none the sandbox path (e.g.
	// /data/ref) is not on the host at all, so a tool that reads one fails with
	// "no such file" — pointing the author at the wrong problem. Refuse the
	// contradiction where it is written (tool-spec §9).
	if t.Sandbox == SandboxNone && len(t.RequiresStorages) > 0 {
		bad("sandbox: none with requires_storages is incompatible: declared storages are " +
			"bind-mounted into a mount namespace, which sandbox: none does not create " +
			"(use sandbox: bwrap)")
	}

	// ── interface ───────────────────────────────────────────────────────
	names := map[string]bool{}
	for i, in := range t.Interface.Inputs {
		where := fmt.Sprintf("interface.inputs[%d] (%s)", i, in.Name)
		if !nameRe.MatchString(in.Name) {
			bad("%s: name must match %s", where, nameRe)
		}
		if names[in.Name] {
			bad("%s: duplicate input name", where)
		}
		names[in.Name] = true

		switch in.Type {
		case TypeString, TypeBool:
		case TypeInt, TypeFloat:
			if in.Min != nil && in.Max != nil && *in.Min > *in.Max {
				bad("%s: min > max", where)
			}
		case TypeEnum:
			if len(in.Values) == 0 {
				bad("%s: type enum requires values", where)
			}
		case TypeFile, TypeDirectory, TypeDirPath:
			for _, st := range splitFrom(in.From) {
				if !declared[st] {
					bad("%s: from %q is not listed in requires_storages", where, st)
				}
			}
		case TypePath:
			if strings.TrimSpace(in.From) == "" {
				bad("%s: type path requires from", where)
			}
			for _, st := range splitFrom(in.From) {
				if !declared[st] {
					bad("%s: from %q is not listed in requires_storages", where, st)
				}
			}
			switch in.Select {
			case "", "file", "directory":
			default:
				bad("%s: select must be file|directory, got %q", where, in.Select)
			}
		default:
			bad("%s: unknown type %q", where, in.Type)
		}

		// A declared default must satisfy its own declaration: a default that
		// violates max/enum would otherwise only fail at submit time, pointing
		// the tool author at the wrong file.
		if in.Default != nil {
			if err := in.CheckValue(in.Default); err != nil {
				bad("%s: default: %v", where, err)
			}
		}
	}

	for i, out := range t.Interface.Outputs {
		where := fmt.Sprintf("interface.outputs[%d] (%s)", i, out.Name)
		if !nameRe.MatchString(out.Name) {
			bad("%s: name must match %s", where, nameRe)
		}
		if names[out.Name] {
			bad("%s: duplicate name (inputs and outputs share one namespace)", where)
		}
		names[out.Name] = true
		switch out.Type {
		case OutFile, OutDirectory:
		default:
			bad("%s: type must be file|directory, got %q", where, out.Type)
		}
	}

	// ── env ──────────────────────────────────────────────────
	// The tool's own environment preparation. The platform's own variables
	// (HOME, SRCOS_*) are set by SRCOS and may not be overridden: a tool that
	// repointed HOME would step outside its virtual home (ADR-021).
	for i, kv := range t.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			bad("env[%d]: want VAR=VALUE, got %q", i, kv)
			continue
		}
		if k == "HOME" || strings.HasPrefix(k, "SRCOS_") {
			bad("env[%d]: %s is reserved — the platform sets it (overriding it would break the sandbox contract)", i, k)
		}
	}

	// ── ro_mounts ───────────────────────────────────────────────────────
	for i, m := range t.ROMounts {
		where := fmt.Sprintf("ro_mounts[%d]", i)
		if !filepath.IsAbs(m.Host) {
			bad("%s: host must be an absolute host path, got %q", where, m.Host)
		}
		if !pathIsAbs(m.SandboxPath) {
			bad("%s: sandbox_path must be absolute, got %q", where, m.SandboxPath)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("tool %s: %d problem(s):\n  - %s", t.ID, len(problems), strings.Join(problems, "\n  - "))
}

func splitFrom(from string) []string {
	if strings.TrimSpace(from) == "" {
		return nil
	}
	parts := strings.Split(from, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func pathIsAbs(p string) bool { return strings.HasPrefix(p, "/") }

// ─────────────────────────────────────────────────────────────────────────
// 资源字符串解析
// ─────────────────────────────────────────────────────────────────────────

var memUnits = []struct {
	suffix string
	mult   uint64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
	// Decimal suffixes must be checked after the binary ones (Ki before K).
	{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
	{"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12},
}

// ParseMemory converts "32Gi" / "512M" / "1024" into bytes.
// Binary suffixes (Ki/Mi/Gi/Ti/Pi) are powers of 1024; decimal suffixes are
// powers of 1000. A bare number is bytes.
func ParseMemory(s string) (uint64, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, fmt.Errorf("empty memory value")
	}
	upper := v
	for _, u := range memUnits {
		if strings.HasSuffix(upper, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(upper, u.suffix))
			n, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid memory %q", s)
			}
			if n < 0 {
				return 0, fmt.Errorf("negative memory %q", s)
			}
			return uint64(n * float64(u.mult)), nil
		}
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q (want e.g. 32Gi, 512M, or bytes)", s)
	}
	return n, nil
}

// FormatMemory renders bytes back into the shortest exact form for systemd.
func FormatMemory(bytes uint64) string {
	if bytes == 0 {
		return "0"
	}
	for _, u := range []struct {
		suffix string
		mult   uint64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if bytes%u.mult == 0 {
			return strconv.FormatUint(bytes/u.mult, 10) + u.suffix
		}
	}
	return strconv.FormatUint(bytes, 10)
}

// ParseWalltime accepts "H:MM:SS" or "MM:SS" (tool-spec §2.1) and returns
// seconds. An empty string yields 0 with no error.
func ParseWalltime(s string) (int64, error) {
	return ParseWalltimeOrZero(s)
}

// ParseWalltimeOrZero is ParseWalltime but treats "" as 0 seconds.
func ParseWalltimeOrZero(s string) (int64, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, nil
	}
	parts := strings.Split(v, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, fmt.Errorf("invalid walltime %q (want H:MM:SS or MM:SS)", s)
	}
	var secs int64
	for _, p := range parts {
		n, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid walltime %q (want H:MM:SS or MM:SS)", s)
		}
		secs = secs*60 + n
	}
	return secs, nil
}

// ─────────────────────────────────────────────────────────────────────────
// command（声明式启动命令）
// ─────────────────────────────────────────────────────────────────────────

// commandVarAllowed reports whether a unit of this kind may reference ${name}.
//
// The rule is "the variables SRCOS actually injects for this kind": a service
// has a port and an instance id, a task has a job root and a task id. Checking
// it at registration turns a typo — or a kind mismatch — into a registration
// error instead of a unit that starts and dies with a confusing message.
func commandVarAllowed(kind Kind, name string) bool {
	if strings.HasPrefix(name, "SRCOS_PARAM_") {
		return true
	}
	switch name {
	case "SRCOS_WORKSPACE", "SRCOS_HOME", "HOME", "SRCOS_JOB_DIR",
		"SRCOS_USER", "SRCOS_TOOL", "SRCOS_TOOL_VERSION", "SRCOS_API":
		return true
	case "SRCOS_PORT", "SRCOS_INSTANCE_ID":
		return kind == KindService
	case "SRCOS_JOB_ROOT", "SRCOS_TASK_ID":
		return kind == KindTask
	}
	return false
}

// commandRefs lists the ${NAME} references in one argument, and reports an
// unterminated one (`${` with no `}`) — a typo that would otherwise reach the
// program as literal text.
func commandRefs(arg string) ([]string, error) {
	var refs []string
	for i := 0; i < len(arg); {
		if arg[i] != '$' || i+1 >= len(arg) || arg[i+1] != '{' {
			i++
			continue
		}
		end := strings.IndexByte(arg[i+2:], '}')
		if end < 0 {
			return nil, fmt.Errorf("unterminated ${ in %q", arg)
		}
		refs = append(refs, arg[i+2:i+2+end])
		i += 2 + end + 1
	}
	return refs, nil
}

// validateCommand checks a declared argv at registration time.
func validateCommand(cmd []string, kind Kind, bad func(string, ...any)) {
	if strings.TrimSpace(cmd[0]) == "" {
		bad("command[0] must name the program to run")
	}
	for i, arg := range cmd {
		refs, err := commandRefs(arg)
		if err != nil {
			bad("command[%d]: %v", i, err)
			continue
		}
		for _, name := range refs {
			if !commandVarAllowed(kind, name) {
				bad("command[%d]: ${%s} is not a variable SRCOS provides to a %s unit", i, name, kind)
			}
		}
	}
}

// ExpandCommand substitutes ${NAME} in a declared argv from the unit's own
// environment.
//
// A reference the environment does not carry is an error, never a silent empty
// string: an empty port would look like "the app started but is unreachable",
// which is the worst failure to debug. The environment is the single source of
// truth, so an expanded command and the process environment cannot disagree.
func ExpandCommand(cmd, env []string) ([]string, error) {
	vars := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			vars[k] = v
		}
	}
	out := make([]string, len(cmd))
	for i, arg := range cmd {
		var b strings.Builder
		for j := 0; j < len(arg); {
			if arg[j] != '$' || j+1 >= len(arg) || arg[j+1] != '{' {
				b.WriteByte(arg[j])
				j++
				continue
			}
			end := strings.IndexByte(arg[j+2:], '}')
			if end < 0 {
				return nil, fmt.Errorf("command[%d]: unterminated ${ in %q", i, arg)
			}
			name := arg[j+2 : j+2+end]
			v, ok := vars[name]
			if !ok {
				return nil, fmt.Errorf("command[%d]: ${%s} is not set for this unit", i, name)
			}
			b.WriteString(v)
			j += 2 + end + 1
		}
		out[i] = b.String()
	}
	return out, nil
}
