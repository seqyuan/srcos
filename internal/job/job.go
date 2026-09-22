// Package job defines the job.json contract: the artifact a tool UI drops
// into $SRCOS_JOB_DIR, plus its validation against the owning tool's interface.
//
// The directory layout is the queue (ADR-004): SRCOS scans for job.json files,
// so any language, anywhere (including a compute node) can submit by writing a
// file. There is no submit daemon and no message broker.
package job

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/tool"
)

// Job is the parsed job.json.
type Job struct {
	SchemaVersion int               `json:"schemaVersion"`
	Name          string            `json:"name"`
	Command       []string          `json:"command,omitempty"`
	Params        map[string]any    `json:"params,omitempty"`
	Resources     *tool.Resources   `json:"resources,omitempty"`
	Outputs       []string          `json:"outputs,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	DoneWhen      *tool.DoneWhen    `json:"doneWhen,omitempty"`
}

// Loaded pairs a parsed Job with where it was found.
type Loaded struct {
	ID   string // directory name = job id
	Dir  string // absolute path of the job directory
	Path string // absolute path of job.json
	Job  *Job
}

// Load reads job.json from a job directory. It does not validate against a
// tool; call Validate for that.
func Load(dir string) (*Job, error) {
	path := filepath.Join(dir, "job.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var j Job
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber() // keep integers exact; float64 loses large ids
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if j.SchemaVersion != 1 {
		return nil, fmt.Errorf("%s: schemaVersion must be 1, got %d", path, j.SchemaVersion)
	}
	if strings.TrimSpace(j.Name) == "" {
		return nil, fmt.Errorf("%s: name is required", path)
	}
	return &j, nil
}

// Scan lists every job directory under jobsDir, sorted by id. A directory
// without a readable job.json is reported as a problem rather than skipped, so
// a malformed submission cannot silently disappear.
type ScanResult struct {
	Jobs   []*Loaded
	Broken []string
}

func Scan(jobsDir string) (*ScanResult, error) {
	entries, err := os.ReadDir(jobsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &ScanResult{}, nil
		}
		return nil, err
	}
	res := &ScanResult{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(jobsDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "job.json")); err != nil {
			continue
		}
		j, err := Load(dir)
		if err != nil {
			res.Broken = append(res.Broken, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		res.Jobs = append(res.Jobs, &Loaded{
			ID:   e.Name(),
			Dir:  dir,
			Path: filepath.Join(dir, "job.json"),
			Job:  j,
		})
	}
	sort.Slice(res.Jobs, func(a, b int) bool { return res.Jobs[a].ID < res.Jobs[b].ID })
	sort.Strings(res.Broken)
	return res, nil
}

// Validate checks the job against its tool's interface and resource ceiling.
//
// Rules from tool-spec §9 and §3:
//   - every param key must be a declared input
//   - every required input must be present (or have a tool-level default)
//   - resources may only be lowered relative to the tool's declaration
//   - outputs must be absolute sandbox paths
//
// Deliberately absent (and rejected if present would be handled by the decoder
// ignoring them): mounts, image, sandbox, backend, env — a job may not change
// its execution environment or add a mount. See tool-spec §3.
func Validate(j *Job, t *tool.Tool) error {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// ── params ──────────────────────────────────────────────────────────
	declared := map[string]tool.Input{}
	for _, in := range t.Interface.Inputs {
		declared[in.Name] = in
	}
	for k := range j.Params {
		if _, ok := declared[k]; !ok {
			bad("params.%s is not declared in the tool interface", k)
		}
	}
	// Satisfiability is judged on the *effective* params: a tool-level default
	// fills in for a value the job omitted.
	eff := EffectiveParams(j, t)
	for _, in := range t.Interface.Inputs {
		v, present := eff[in.Name]
		if !present {
			if in.Required {
				bad("required param %q is missing and the input declares no default", in.Name)
			}
			continue
		}
		if err := in.CheckValue(v); err != nil {
			bad("params.%s: %v", in.Name, err)
		}
	}

	// ── resources: lower-only ───────────────────────────────────────────
	if j.Resources != nil {
		if j.Resources.CPU < 0 {
			bad("resources.cpu must not be negative")
		}
		if j.Resources.CPU > 0 && t.Resources.CPU > 0 && j.Resources.CPU > t.Resources.CPU {
			bad("resources.cpu %d exceeds the tool ceiling %d", j.Resources.CPU, t.Resources.CPU)
		}
		if j.Resources.Memory != "" {
			want, err := tool.ParseMemory(j.Resources.Memory)
			if err != nil {
				bad("resources.memory: %v", err)
			} else if ceiling, err := tool.ParseMemory(t.Resources.Memory); err == nil && want > ceiling {
				bad("resources.memory %s exceeds the tool ceiling %s", j.Resources.Memory, t.Resources.Memory)
			}
		}
		if j.Resources.Walltime != "" {
			want, err := tool.ParseWalltime(j.Resources.Walltime)
			if err != nil {
				bad("resources.walltime: %v", err)
			} else if ceiling, err := tool.ParseWalltime(t.Resources.Walltime); err == nil && ceiling > 0 && want > ceiling {
				bad("resources.walltime %s exceeds the tool ceiling %s", j.Resources.Walltime, t.Resources.Walltime)
			}
		}
	}

	// ── outputs ─────────────────────────────────────────────────────────
	for i, o := range j.Outputs {
		if !strings.HasPrefix(o, "/") {
			bad("outputs[%d] %q must be an absolute sandbox path", i, o)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("job %q: %d problem(s):\n  - %s", j.Name, len(problems), strings.Join(problems, "\n  - "))
}

func isString(v any) bool { _, ok := v.(string); return ok }

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 环境变量投影
// ─────────────────────────────────────────────────────────────────────────

// ParamEnvName maps an interface input name to its injected environment
// variable: uppercase, '-' -> '_', prefixed with SRCOS_PARAM_ (tool-spec §4.1).
func ParamEnvName(name string) string {
	up := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return "SRCOS_PARAM_" + up
}

// EffectiveParams is what the tool will actually see: every declared input
// carrying its tool-level default, overridden by whatever the job supplied.
//
// This is the only place defaults are applied. A tool that declares
// `default: "S001,S002"` gets that value even when the job omits the key, so
// `work.sh` never has to guess.
func EffectiveParams(j *Job, t *tool.Tool) map[string]any {
	out := make(map[string]any, len(t.Interface.Inputs))
	for _, in := range t.Interface.Inputs {
		if in.Default != nil {
			out[in.Name] = in.Default
		}
	}
	for k, v := range j.Params {
		if v == nil {
			continue
		}
		// An empty string means "left blank in the form", which falls back to
		// the declared default rather than blanking it out.
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		out[k] = v
	}
	return out
}

// ParamEnv renders params as SRCOS_PARAM_* environment entries.
//
// Values are rendered as plain strings; the tool parses numbers and booleans
// itself, matching the shell-first contract.
func ParamEnv(params map[string]any) []string {
	names := make([]string, 0, len(params))
	for k := range params {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic ordering for tests and audit logs

	env := make([]string, 0, len(names))
	for _, k := range names {
		env = append(env, ParamEnvName(k)+"="+asString(params[k]))
	}
	return env
}

// EffectiveResources merges the job's overrides onto the tool's declaration.
// Only lowering is allowed, so a plain min() is the correct semantics.
func EffectiveResources(j *Job, t *tool.Tool) tool.Resources {
	out := t.Resources
	if j.Resources == nil {
		return out
	}
	if j.Resources.CPU > 0 && (out.CPU == 0 || j.Resources.CPU < out.CPU) {
		out.CPU = j.Resources.CPU
	}
	if j.Resources.Memory != "" {
		if want, err := tool.ParseMemory(j.Resources.Memory); err == nil {
			if ceiling, err := tool.ParseMemory(out.Memory); err == nil && want < ceiling {
				out.Memory = j.Resources.Memory
			}
		}
	}
	if j.Resources.Walltime != "" {
		if want, err := tool.ParseWalltime(j.Resources.Walltime); err == nil {
			if ceiling, err := tool.ParseWalltime(out.Walltime); err == nil && (ceiling == 0 || want < ceiling) {
				out.Walltime = j.Resources.Walltime
			}
		}
	}
	if j.Resources.GPU > 0 {
		out.GPU = j.Resources.GPU
	}
	return out
}
