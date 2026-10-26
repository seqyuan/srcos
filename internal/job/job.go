// Package job defines the job.json contract: the artifact a tool UI drops
// into $SRCOS_JOB_DIR, plus its validation against the owning tool's interface.
//
// The directory layout is the queue (ADR-004): SRCOS scans for job.json files,
// so any language, anywhere (including a compute node) can submit by writing a
// file. There is no submit daemon and no message broker.
package job

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/safefs"
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
		// GPU is a real scheduler resource on SGE (`-l gpu=N`), so the
		// lower-only rule applies to it exactly as it does to CPU and memory.
		if j.Resources.GPU < 0 {
			bad("resources.gpu must not be negative")
		} else if j.Resources.GPU > t.Resources.GPU {
			bad("resources.gpu %d exceeds the tool ceiling %d", j.Resources.GPU, t.Resources.GPU)
		}
		if j.Resources.HMem != "" || j.Resources.VMem != "" {
			bad("resources.h_vmem/v_mem are not supported; use resources.memory")
		}
	}

	// A job may override the tool's completion probe, but it must still be a
	// valid probe: an unknown type used to count any existing path as success.
	if err := tool.ValidateDoneWhen(j.DoneWhen); err != nil {
		bad("doneWhen: %v", err)
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
	if j == nil || j.Resources == nil {
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
	if j.Resources.GPU > 0 && j.Resources.GPU <= out.GPU {
		out.GPU = j.Resources.GPU
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// 提交
// ─────────────────────────────────────────────────────────────────────────

// Slug converts a display name into a filesystem- and URL-safe fragment.
//
// Job ids end up in directory names, systemd unit names and URLs, so the
// alphabet is deliberately narrow: lowercase alphanumerics and single hyphens.
func Slug(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "job"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// NewID allocates a unique job id under jobsDir.
//
// The random suffix makes concurrent submissions from different browsers safe
// without a lock, and keeps ids stable enough to type: "<slug>-<8 hex>".
func NewID(jobsDir, name string) string {
	slug := Slug(name)
	for attempt := 0; attempt < 8; attempt++ {
		id := slug + "-" + randomSuffix()
		if _, err := os.Stat(filepath.Join(jobsDir, id)); os.IsNotExist(err) {
			return id
		}
	}
	// Extremely unlikely; fall back to something certainly unique.
	return fmt.Sprintf("%s-%d", slug, time.Now().UnixNano())
}

func randomSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// claimFile is the *legacy* claim marker inside the job directory.
//
// Older builds wrote it with O_EXCL and never refreshed it. The authoritative
// lease now lives in data/claims/<user>/<tool>/<job>.lock, outside every sandbox
// mount, because a marker in a tool-writable directory can be deleted by the
// tool. The legacy file is only read for one migration window; see Claim.
const claimFile = ".srcos-claimed"

// LegacyClaimPath is the old marker's location (inside the job directory).
func LegacyClaimPath(jobDir string) string { return filepath.Join(jobDir, claimFile) }

// ClaimPair derives the authoritative lease path and the legacy marker path for
// one job. Every caller uses this, so the two derivations cannot drift.
func ClaimPair(configDir, user, toolID, jobID, jobDir string) (claimPath, legacyPath string) {
	return config.ClaimPath(configDir, user, toolID, jobID), LegacyClaimPath(jobDir)
}

// LegacyClaimMaxAge bounds how long a marker left by an old (pre-flock) build is
// honored. The old build wrote it once and never refreshed it, so a live old
// runner can hold it for the whole job; the job's declared walltime plus a
// margin is the honest upper bound. Unknown/zero walltime falls back to 12h.
func LegacyClaimMaxAge(t *tool.Tool, j *Job) time.Duration {
	res := EffectiveResources(j, t)
	if secs, err := tool.ParseWalltime(res.Walltime); err == nil && secs > 0 {
		return time.Duration(secs)*time.Second + 10*time.Minute
	}
	return 12 * time.Hour
}

// Lease is a held claim. Release (or process exit) frees it; the file itself
// stays as a human-readable hint.
type Lease struct {
	file *os.File
	once sync.Once
}

// Release unlocks and closes the lease. It is safe to call more than once.
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.file != nil {
			_ = unlockFile(l.file)
			_ = l.file.Close()
			l.file = nil
		}
	})
}

// Claim tries to take the lease for one submission. A non-nil lease means this
// caller owns it until Release. A nil lease with a nil error means another
// runner holds it (or a live old build still owns the legacy marker).
//
// The decision is the flock on claimPath. The legacy marker cannot be locked
// (old builds never took a lock), so it is honored only while it is younger
// than legacyMaxAge; otherwise it is treated as a crashed old runner and
// removed best-effort. Running two binary versions against one data/ is not a
// supported state; this window exists to survive a rolling restart.
func Claim(claimPath, legacyPath string, legacyMaxAge time.Duration) (*Lease, error) {
	if strings.TrimSpace(claimPath) == "" {
		return nil, errors.New("empty claim path")
	}
	if err := os.MkdirAll(filepath.Dir(claimPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(claimPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errLeaseHeld) {
			return nil, nil
		}
		return nil, err
	}
	if legacyFresh(legacyPath, legacyMaxAge) {
		// A live old build owns it. Give the new lock back; the old runner is
		// still working and its instance record will protect the next ticks.
		_ = unlockFile(f)
		_ = f.Close()
		return nil, nil
	}
	if legacyPath != "" {
		// Stale (or absent): remove it best-effort. A read-only job directory
		// must not make the new lease unusable.
		_ = os.Remove(legacyPath)
	}
	// We hold the lock: refresh the human-readable hint. A write failure does
	// not invalidate the lease (the lock is the decision), so keep it.
	_ = f.Truncate(0)
	if _, err := f.Seek(0, 0); err == nil {
		_, _ = fmt.Fprintf(f, "claimed by pid %d at %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	}
	return &Lease{file: f}, nil
}

// Claimed reports whether another runner currently holds this submission's
// lease. It is a best-effort early check: Claim is the decision.
//
// It never creates the legacy marker (an old binary would then see it and park
// the job forever), and an unreadable legacy path is treated as stale.
func Claimed(claimPath, legacyPath string, legacyMaxAge time.Duration) bool {
	if claimPath != "" {
		f, err := os.OpenFile(claimPath, os.O_RDONLY, 0)
		if err == nil {
			if err := lockFile(f); err != nil {
				_ = f.Close()
				if errors.Is(err, errLeaseHeld) {
					return true
				}
			} else {
				_ = unlockFile(f)
				_ = f.Close()
			}
		}
	}
	return legacyFresh(legacyPath, legacyMaxAge)
}

// legacyFresh reports whether an old-build marker is still young enough to be
// honored. Lstat (not Stat) keeps a symlink planted by a tool from pointing at
// an unrelated file; any stat error is treated as stale, never as held.
func legacyFresh(path string, maxAge time.Duration) bool {
	if path == "" || maxAge <= 0 {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) <= maxAge
}

// Submit writes a job into the drop-box and returns its id.
//
// The directory is the queue (ADR-004): writing the file *is* the submission,
// which is what lets any language, anywhere — including a compute node — submit
// without a daemon, a socket, or a credential.
// ErrJobExists means a job already exists under a caller-chosen id. It is the
// signal an idempotent submission replays on, never an error to surface.
var ErrJobExists = errors.New("a job already exists under this id")

// IdempotentID derives a stable job id from a client-supplied key, so the same
// key always names the same job directory. The key is namespaced by user and
// tool: two callers may pick the same string without colliding. The "k-"
// prefix distinguishes a keyed id from a random one at a glance.
func IdempotentID(user, toolID, key string) string {
	sum := sha256.Sum256([]byte(user + "\x00" + toolID + "\x00" + key))
	return "k-" + hex.EncodeToString(sum[:12])
}

func Submit(configDir, user, toolID string, j *Job) (string, string, error) {
	return SubmitAs(configDir, user, toolID, j, "")
}

// SubmitAs files a job under a caller-chosen id. An id of "" allocates one.
//
// A non-empty id that already holds a job.json is reported as ErrJobExists
// rather than overwritten: a caller-chosen id is an idempotency key, so finding
// one there means this submission already happened.
// SubmitScript files a job whose run script comes from the caller, without a
// window in which the queue can see half a submission.
//
// The ordinary path writes job.json first (that is what makes a directory a
// submission), so writing work.sh afterwards leaves a race: the gateway can
// claim the job before the script exists. Here the script is written first and
// job.json last, which is the publication step. job.json.cmd is set as well so
// the submission remains self-describing even if the script is later removed.
func SubmitScript(configDir, user, toolID string, j *Job, script []byte) (string, string, error) {
	jobsDir := config.JobsDir(configDir, user, toolID)
	if err := safefs.MkdirAllBeneath(config.DataDir(configDir), jobsDir, 0o755); err != nil {
		return "", "", err
	}
	id := NewID(jobsDir, j.Name)
	dir := filepath.Join(jobsDir, id)
	if err := safefs.MkdirAllBeneath(config.DataDir(configDir), dir, 0o755); err != nil {
		return "", "", err
	}
	if j.Command == nil {
		j.Command = []string{"bash", "work.sh"}
	}
	if err := safefs.WriteFileBeneath(config.DataDir(configDir), filepath.Join(dir, "work.sh"), script, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if j.SchemaVersion == 0 {
		j.SchemaVersion = 1
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	if err := safefs.WriteFileAtomicSynced(filepath.Join(dir, "job.json"), append(data, '\n'), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return id, dir, nil
}

func SubmitAs(configDir, user, toolID string, j *Job, id string) (string, string, error) {
	jobsDir := config.JobsDir(configDir, user, toolID)
	if err := safefs.MkdirAllBeneath(config.DataDir(configDir), jobsDir, 0o755); err != nil {
		return "", "", err
	}
	if id == "" {
		id = NewID(jobsDir, j.Name)
	}
	dir := filepath.Join(jobsDir, id)
	if _, err := os.Stat(filepath.Join(dir, "job.json")); err == nil {
		return id, dir, ErrJobExists
	}
	if err := safefs.MkdirAllBeneath(config.DataDir(configDir), dir, 0o755); err != nil {
		return "", "", err
	}
	if j.SchemaVersion == 0 {
		j.SchemaVersion = 1
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := safefs.WriteFileAtomicExclusive(filepath.Join(dir, "job.json"), append(data, '\n'), 0o644); err != nil {
		if errors.Is(err, os.ErrExist) {
			return id, dir, ErrJobExists
		}
		return "", "", err
	}
	return id, dir, nil
}

// ProbeLease verifies that this filesystem can arbitrate leases: it creates two
// descriptors for one file, takes the flock on the first and expects the second
// to report "held". run doctor uses it because a filesystem without flock would
// make every job unclaimable.
func ProbeLease(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, ".lease-probe")
	f1, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	defer f1.Close()
	if err := lockFile(f1); err != nil {
		return fmt.Errorf("first flock: %w", err)
	}
	defer unlockFile(f1)
	f2, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f2.Close()
	err = lockFile(f2)
	if err == nil {
		_ = unlockFile(f2)
		return errors.New("flock did not arbitrate: a second descriptor took the lock")
	}
	if !errors.Is(err, errLeaseHeld) {
		return fmt.Errorf("second flock: %w", err)
	}
	return nil
}
