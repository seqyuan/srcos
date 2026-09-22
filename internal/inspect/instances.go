package inspect

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the instance half of the read side: what the user's jobs and
// services are doing, what they wrote, and what they said while doing it.
//
// Every answer is scoped to one user. The instance records live in one flat
// directory on purpose (data/instances is the queue and the history), so the
// scoping happens here, at the only place that hands records out.

// DefaultLogTail / MaxLogTail bound the log answer, in lines.
const (
	DefaultLogTail = 200
	MaxLogTail     = 5000
)

// InstanceView is one instance as a caller sees it.
//
// The json tags are the wire contract: the REST API and the MCP tool both
// serve this shape, so an agent reading a job list and a browser reading
// /api/jobs cannot disagree about a field's name.
type InstanceView struct {
	ID        string            `json:"id"`
	Tool      string            `json:"tool"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	State     string            `json:"state"`
	ExitCode  int               `json:"exitCode"`
	Error     string            `json:"error,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	RoutePath string            `json:"routePath,omitempty"`
	Backend   string            `json:"backend"`
	Sandbox   string            `json:"sandbox"`
	Limiter   string            `json:"limiter,omitempty"`
	Duration  string            `json:"duration,omitempty"`
	StartedAt string            `json:"startedAt,omitempty"`
	Outputs   []string          `json:"outputs,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

func viewFromInstance(i *runtime.Instance) InstanceView {
	v := InstanceView{
		ID:        i.ID,
		Tool:      i.Tool,
		Kind:      i.Kind,
		Name:      i.JobName,
		State:     string(i.State),
		ExitCode:  i.ExitCode,
		Error:     i.Error,
		Endpoint:  i.Endpoint,
		RoutePath: i.RoutePath,
		Backend:   i.Backend,
		Sandbox:   i.Sandbox,
		Limiter:   i.Limiter,
		Duration:  i.Duration,
		Outputs:   i.Outputs,
		Tags:      i.Tags,
	}
	if !i.StartedAt.IsZero() {
		v.StartedAt = i.StartedAt.Format(time.RFC3339)
	}
	return v
}

// Instances lists this user's instances, newest first, optionally filtered.
func (r *Reader) Instances(username, toolFilter, kindFilter string) ([]InstanceView, error) {
	records, err := r.records(username)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceView, 0, len(records))
	for _, i := range records {
		if toolFilter != "" && i.Tool != toolFilter {
			continue
		}
		if kindFilter != "" && i.Kind != kindFilter {
			continue
		}
		out = append(out, viewFromInstance(i))
	}
	return out, nil
}

// Instance returns one of this user's instances.
//
// The argument may be the full instance id, a job id, or any unambiguous
// suffix of either: an agent that got an id from a list should not have to
// reconstruct `alice-hello-fanout-20260922-141500`. An ambiguous suffix is an
// error listing the candidates, never a guess.
func (r *Reader) Instance(username, needle string) (*InstanceView, []string, error) {
	rec, other, err := r.record(username, needle)
	if err != nil {
		return nil, other, err
	}
	v := viewFromInstance(rec)
	return &v, nil, nil
}

// record is the instance-scoped lookup behind every answer in this file.
func (r *Reader) record(username, needle string) (*runtime.Instance, []string, error) {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return nil, nil, fmt.Errorf("%w: an instance id is required", ErrBadRequest)
	}
	records, err := r.records(username)
	if err != nil {
		return nil, nil, err
	}

	var matches []*runtime.Instance
	for _, i := range records {
		if i.ID == needle {
			return i, nil, nil
		}
		if strings.HasSuffix(i.ID, "-"+needle) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("%w: no instance matching %q", ErrNotFound, needle)
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ID)
		}
		sort.Strings(ids)
		return nil, ids, fmt.Errorf("%w: %q matches %d instances, use the full id", ErrBadRequest, needle, len(ids))
	}
	return matches[0], nil, nil
}

// records returns one user's instance records, newest first.
func (r *Reader) records(username string) ([]*runtime.Instance, error) {
	if r.ConfigDir == "" {
		return nil, fmt.Errorf("%w: no instance store is configured", ErrUnavailable)
	}
	all, err := runtime.ListInstances(r.ConfigDir)
	if err != nil {
		return nil, err
	}
	out := make([]*runtime.Instance, 0, len(all))
	for _, i := range all {
		if i.User == username {
			out = append(out, i)
		}
	}
	return out, nil
}

// Logs returns the tail of an instance's log.
//
// It reads the copy SRCOS itself wrote (the record's LogPath, which lives
// outside the workspace on purpose): a tool cannot rewrite its own record.
func (r *Reader) Logs(username, needle string, tail int) (string, error) {
	rec, _, err := r.record(username, needle)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = DefaultLogTail
	}
	if tail > MaxLogTail {
		tail = MaxLogTail
	}
	data, err := os.ReadFile(rec.LogPath)
	if err != nil {
		return "", fmt.Errorf("%w: no log for %s", ErrNotFound, rec.ID)
	}
	return tailLines(string(data), tail), nil
}

// tailLines returns the last n lines, with a marker when the read was
// truncated at the front.
func tailLines(s string, n int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) <= n {
		return s
	}
	kept := lines[len(lines)-n:]
	return fmt.Sprintf("... (%d earlier lines omitted)\n%s\n", len(lines)-n, strings.Join(kept, "\n"))
}

// Artifact is one declared output of a task instance, resolved.
type Artifact struct {
	// Path is the sandbox path the tool declared — the value a follow-up call
	// (read_file) can use.
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	IsDir  bool   `json:"isDir,omitempty"`
	Size   int64  `json:"size,omitempty"`
	MTime  string `json:"mtime,omitempty"`
	// Entries counts a directory's immediate children.
	Entries int `json:"entries,omitempty"`
	// Note explains an artifact that is not there, in the instance's own words
	// ("missing", "unresolvable", "escapes its mount").
	Note string `json:"note,omitempty"`
}

// Artifacts resolves the outputs a task instance declared.
//
// The list comes from the instance record (and before that from job.json), so
// what is reported is what the submitter asked for — not a directory walk,
// which would hand back whatever the tool happened to leave behind, including
// temporary files.
func (r *Reader) Artifacts(username, needle string) ([]Artifact, error) {
	rec, _, err := r.record(username, needle)
	if err != nil {
		return nil, err
	}
	out := make([]Artifact, 0, len(rec.Outputs))
	if len(rec.Outputs) == 0 {
		return out, nil
	}

	// The mount table is the only definition of "what this instance can see",
	// so resolution reuses the runtime's own builder instead of a second copy
	// of the mount rules.
	spec, note := r.instanceSpec(rec)
	for _, declared := range rec.Outputs {
		path, recorded := splitAnnotation(declared)
		a := Artifact{Path: path}
		if spec == nil {
			a.Note = note
			out = append(out, a)
			continue
		}
		host, _, err := spec.Resolve(path)
		if err != nil {
			a.Note = firstNote(recorded, "unresolvable")
			out = append(out, a)
			continue
		}
		fi, err := os.Stat(host)
		if err != nil {
			a.Note = firstNote(recorded, "missing")
			out = append(out, a)
			continue
		}
		// A symlink inside a writable mount can point out of it; the artifact
		// is reported but flagged rather than quietly attributed to the mount.
		if _, err := spec.ResolveExisting(path); err != nil {
			a.Note = "escapes its mount (symlink)"
			out = append(out, a)
			continue
		}
		a.Exists = true
		a.IsDir = fi.IsDir()
		a.MTime = fi.ModTime().Format(time.RFC3339)
		if fi.IsDir() {
			if entries, err := os.ReadDir(host); err == nil {
				a.Entries = len(entries)
			}
		} else {
			a.Size = fi.Size()
		}
		out = append(out, a)
	}
	return out, nil
}

// instanceSpec rebuilds the mount table of an instance, so a sandbox path in
// its record can be resolved back to a host path. A missing tool package or an
// undeclared storage is reported as a note rather than failing the whole call:
// the rest of the answer is still useful.
func (r *Reader) instanceSpec(rec *runtime.Instance) (*sandbox.Spec, string) {
	if r.ToolsDir == "" {
		return nil, "no tool directory is configured"
	}
	t, err := tool.Find(r.ToolsDir, rec.Tool)
	if err != nil {
		return nil, "tool package is gone"
	}
	jobID := ""
	if rec.Kind == string(tool.KindTask) {
		if rest, ok := strings.CutPrefix(rec.ID, rec.User+"-"+rec.Tool+"-"); ok {
			jobID = rest
		}
	}
	paths := runtime.PathsFor(r.ConfigDir, rec.User, rec.Tool, jobID)
	spec, err := runtime.BuildSpec(t, paths, r.Storages)
	if err != nil {
		return nil, "mount table unavailable: " + err.Error()
	}
	return spec, ""
}

// outputNotes are the annotations the runtime appends when it reports outputs.
//
// Only these two are stripped, by exact suffix: a declared path may legitimately
// contain " (" and end with ")" (a versioned directory, say), and treating any
// such name as an annotation would silently rewrite what the submitter asked
// for.
var outputNotes = []string{" (missing)", " (unresolvable)"}

// splitAnnotation separates a recorded path from the note the runtime appended.
func splitAnnotation(declared string) (path, note string) {
	for _, suffix := range outputNotes {
		if strings.HasSuffix(declared, suffix) {
			return strings.TrimSpace(strings.TrimSuffix(declared, suffix)), strings.Trim(suffix, " ()")
		}
	}
	return declared, ""
}

func firstNote(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
