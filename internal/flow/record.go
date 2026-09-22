package flow

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// This file is the run record: what a flow run did, and what it still has to do.
//
// Two authorities, deliberately:
//
//   - **the instance records** are the truth about one job (did it run, what did
//     it exit with, what did it write) — SRCOS already keeps them, and a flow
//     must not keep a second copy;
//   - **the node's `.sign` file** is the truth about "this step is done", and it
//     is *human-writable*: `touch` it and the run moves on (annopi's `is_signed`).
//     That escape hatch is the only sane answer when a step cannot be re-run.
//
// So this record holds the *index*: which run, which flow version, which nodes,
// which jobs, and the last decision made about each node.

// Node states in the record.
const (
	NodePending   = "pending"
	NodeRunning   = "running"
	NodeSucceeded = "succeeded"
	NodeFailed    = "failed"
	NodeSkipped   = "skipped" // upstream failed and the node is not `when: always`
)

// Run states.
const (
	RunRunning   = "running"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// CancelName is the flag file that asks a running flow to stop scheduling.
//
// A file rather than a signal or an API call: the process running a flow may be
// a CLI invocation in someone's terminal, and `flow cancel` runs in another
// process entirely. A flag file is the same "filesystem is the database" idiom
// the rest of SRCOS uses, and it is inspectable ("is this run being cancelled?"
// is `ls`).
const CancelName = "cancel"

// NodeState is one node's progress in a run.
type NodeState struct {
	ID    string `yaml:"id"`
	State string `yaml:"state"`
	// JobIDs are the submitted jobs for this node, in submission order: a
	// retried unit appends another id, so the node's history is the list.
	JobIDs []string `yaml:"job_ids,omitempty"`
	// Units is how many attempts each sample unit has had (keyed by the sample
	// segment). It is persisted so a resumed run does not restart a retry
	// budget from zero.
	Units map[string]int `yaml:"units,omitempty"`
	// Attempts counts how many times this node was submitted (a resumed run
	// increments it), so a retry loop is visible rather than mysterious.
	Attempts  int       `yaml:"attempts,omitempty"`
	Error     string    `yaml:"error,omitempty"`
	StartedAt time.Time `yaml:"started_at,omitempty"`
	EndedAt   time.Time `yaml:"ended_at,omitempty"`
}

// AddUnitAttempt records one attempt of one sample unit.
func (n *NodeState) AddUnitAttempt(segment string) {
	if n.Units == nil {
		n.Units = map[string]int{}
	}
	n.Units[segment]++
}

// UnitAttempts returns how many attempts a sample unit has had.
func (n *NodeState) UnitAttempts(segment string) int {
	if n == nil {
		return 0
	}
	return n.Units[segment]
}

// Run is the persisted state of one flow run.
type Run struct {
	FlowID      string `yaml:"flow_id"`
	FlowVersion string `yaml:"flow_version"`
	User        string `yaml:"user"`
	ID          string `yaml:"id"`
	// Samples is how many rows the run expanded.
	Samples int `yaml:"samples"`
	// Params are the user-supplied values, kept so a resume does not need them
	// again (and so the run is reproducible from its own directory).
	Params    map[string]string `yaml:"params,omitempty"`
	State     string            `yaml:"state"`
	StartedAt time.Time         `yaml:"started_at"`
	EndedAt   time.Time         `yaml:"ended_at,omitempty"`
	Nodes     []NodeState       `yaml:"nodes"`
}

// Node returns a node's state, creating it as pending when it is missing (a run
// record written by an older plan, or a hand-edited file).
func (r *Run) Node(id string) *NodeState {
	for i := range r.Nodes {
		if r.Nodes[i].ID == id {
			return &r.Nodes[i]
		}
	}
	r.Nodes = append(r.Nodes, NodeState{ID: id, State: NodePending})
	return &r.Nodes[len(r.Nodes)-1]
}

// SaveRun writes the record atomically: a run's state is read by the next
// `flow resume` (and by a human), so it must never be half-written.
func SaveRun(path string, r *Run) error {
	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	header := "# SRCOS 流程运行记录（flowrun.yaml）\n" +
		"#\n" +
		"# 由 `srcos flow run` 维护；节点完成以 nodes/<节点>/.sign 为准（可以手工 touch 跳过）。\n" +
		"# 这里只记索引：真正的任务状态在 data/instances/ 的实例记录里。\n\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), data...), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadRun reads a run record.
func LoadRun(path string) (*Run, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// IsSigned reports whether a node's completion marker exists.
//
// The marker wins over the record: an operator who touched it has decided, and
// second-guessing that would make the escape hatch useless.
func IsSigned(signPath string) bool {
	info, err := os.Stat(signPath)
	return err == nil && !info.IsDir()
}

// CancelPath is the flag file a `flow cancel` writes.
func CancelPath(dataDir, user, runID string) string {
	return filepath.Join(RunDir(dataDir, user, runID), CancelName)
}

// IsCancelled reports whether a run has been asked to stop scheduling.
func IsCancelled(cancelPath string) bool {
	info, err := os.Stat(cancelPath)
	return err == nil && !info.IsDir()
}

// RequestCancel writes the cancel flag (creating the run directory if needed).
func RequestCancel(cancelPath string) error {
	if err := os.MkdirAll(filepath.Dir(cancelPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(cancelPath)
	if err != nil {
		return err
	}
	return f.Close()
}

// Sign writes the node's completion marker.
func Sign(signPath string) error {
	if err := os.MkdirAll(filepath.Dir(signPath), 0o755); err != nil {
		return err
	}
	f, err := os.Create(signPath)
	if err != nil {
		return err
	}
	return f.Close()
}

// Summary renders one line per node, for `flow run` output and `flow status`.
func (r *Run) Summary() string {
	out := fmt.Sprintf("run %s · flow %s@%s · user %s · %d sample(s) · %s",
		r.ID, r.FlowID, r.FlowVersion, r.User, r.Samples, r.State)
	for _, n := range r.Nodes {
		detail := ""
		if len(n.JobIDs) > 0 {
			detail = fmt.Sprintf(" (%d job(s))", len(n.JobIDs))
		}
		if n.Error != "" {
			detail += " — " + n.Error
		}
		out += fmt.Sprintf("\n  %-14s %s%s", n.ID, n.State, detail)
	}
	return out
}
