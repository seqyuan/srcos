package flow

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// This file is the run workspace: where a flow's wires actually meet.
//
// A flow's edges pass *paths* between tools, and two tools can only share a path
// if the platform puts it somewhere both can see. SRCOS does not let a job add
// mounts (tool-spec §3: that is the security boundary), and it does not
// template paths (a template language would bypass the interface type checking).
// So the platform owns the paths:
//
//	/flow/runs/<run>/nodes/<node>/<sample>/<output>       (sandbox view)
//	<data>/flows/<user>/runs/<run>/nodes/<node>/<sample>/<output>   (host view)
//
// `/flow` is a *builtin* mount, exactly like `/workspace` and `/home/<user>`
// (ADR-021): it is the user's own runtime state, created by SRCOS, not a data
// storage an administrator declares (ADR-020). Consequences worth naming:
//
//   - a flow author never writes a path, so per-sample directories are free and
//     no `${...}` syntax is needed;
//   - the run is reproducible: every path is derived from the run id, so
//     resuming a run reuses the same directories and the same inputs;
//   - it works with `ro` storage: nothing needs to be writable except the
//     platform's own run directory.

// PathFlow is the sandbox mount point for the user's flow runs. It joins the
// well-known paths of the tool contract (tool-spec §8).
const PathFlow = "/flow"

// RunsSubdir is the directory (under the flow mount) holding every run.
const RunsSubdir = "runs"

// RecordName is the run record inside a run directory.
const RecordName = "flowrun.yaml"

// SamplesCopyName is the archived copy of the submitted sample table.
const SamplesCopyName = "samples.csv"

// SignName is the file whose existence means "this node is done".
//
// It is annopi's escape hatch (`is_signed`), kept because it is the one thing an
// operator needs when a step cannot be re-run: `touch` it and the run moves on.
// The file is the authority — a hand-touched sign beats the record.
const SignName = ".sign"

// FlowDir is the host directory holding one user's flow runs.
func FlowDir(dataDir, user string) string {
	return filepath.Join(dataDir, "flows", user)
}

// RunsDir is the host directory holding every run of one user.
func RunsDir(dataDir, user string) string {
	return filepath.Join(FlowDir(dataDir, user), RunsSubdir)
}

// RunDir is one run's host directory.
func RunDir(dataDir, user, runID string) string {
	return filepath.Join(RunsDir(dataDir, user), runID)
}

// NodeDir is one (run, node) host directory: where per-node bookkeeping lives
// (the run's sign file, and the node's per-sample working directories).
func NodeDir(dataDir, user, runID, nodeID string) string {
	return filepath.Join(RunDir(dataDir, user, runID), "nodes", nodeID)
}

// SignPath is the node's completion marker on the host.
func SignPath(dataDir, user, runID, nodeID string) string {
	return filepath.Join(NodeDir(dataDir, user, runID, nodeID), SignName)
}

// SampleDir is one (run, node, sample) host directory: the working area a tool
// gets for one unit of work.
func SampleDir(dataDir, user, runID, nodeID, sample string) string {
	return filepath.Join(NodeDir(dataDir, user, runID, nodeID), sample)
}

// RecordPath is the run record on the host.
func RecordPath(dataDir, user, runID string) string {
	return filepath.Join(RunDir(dataDir, user, runID), RecordName)
}

// ─────────────────────────────────────────────────────────────────────────
// 沙箱路径（交给工具与 job.json 的那一侧）
// ─────────────────────────────────────────────────────────────────────────

// SandboxSampleDir is the sandbox path of one (run, node, sample) working area.
func SandboxSampleDir(runID, nodeID, sample string) string {
	return strings.Join([]string{PathFlow, RunsSubdir, slug(runID), "nodes", slug(nodeID), sample}, "/")
}

// SandboxOutput is where a node's declared output lands for one sample.
//
// The output's *name* (from the tool's interface) is the file or directory name,
// which is what makes a binding resolvable without the flow author writing any
// path: `count.outputs.outs` is `<sample dir>/outs` by construction.
func SandboxOutput(runID, nodeID, sample, output string) string {
	return SandboxSampleDir(runID, nodeID, sample) + "/" + slug(output)
}

// SampleSegment renders the directory segment of one sample row.
//
// It carries both the row number and the sample's name: the number guarantees
// uniqueness (two rows may legitimately share a sample name, and a duplicate
// directory would make one unit overwrite another's outputs), and the name makes
// a run directory readable — which is the whole point of keeping these paths
// around for debugging.
func SampleSegment(index int, sampleID string) string {
	segment := fmt.Sprintf("s%02d", index+1)
	if id := slug(sampleID); id != "" {
		segment += "-" + id
	}
	return segment
}

// NewRunID mints a run id: readable, sortable, and unique enough for one
// operator's morning.
func NewRunID(flowID string, now time.Time) string {
	// Lowercase throughout: a run id is a path segment and must satisfy the
	// same slug rule as a node id (and it stays readable in the run list).
	stamp := now.UTC().Format("20060102-150405")
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%s", slug(flowID), stamp)
	}
	return fmt.Sprintf("%s-%s-%s", slug(flowID), stamp, hex.EncodeToString(b[:]))
}

// ValidRunID reports whether an id may be used as a path segment.
func ValidRunID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	return idRe.MatchString(id)
}

// slug keeps an identifier safe as a single path segment: node ids and run ids
// are already restricted, but a sample id comes from a user's table and may
// contain anything at all (spaces, slashes, dots, unicode).
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_' || r == '-' || r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
			// Anything else (unicode, punctuation, a path separator) is
			// dropped: the result is only ever a directory name, and dropping
			// is safer than escaping a separator into something that looks
			// like a path.
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return ""
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
