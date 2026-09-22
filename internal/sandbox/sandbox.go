// Package sandbox turns a declarative mount list into a materialized sandbox
// view, and provides the single path-resolution entry point for anything that
// maps a sandbox path back to a host path.
//
// Two distinct boundaries live around this package (see AGENTS.md):
//
//   - MountSpec decides what a sandboxed *process* can see. Its granularity is
//     the granularity of isolation: because bwrap's user namespace collapses
//     every unmapped gid to 65534 — and the sandboxed process is itself in
//     group 65534 — any host `group` permission bit is effectively public
//     inside the sandbox. Binding a parent directory therefore hands over
//     everything under it that is group-readable. Never bind a parent.
//   - Resolve/Display are the Jail for SRCOS's *own* API (path listing, file
//     preview, MCP). They protect SRCOS, not the sandbox.
//
// Mixing the two up produces false confidence, so both are documented here and
// kept in one place.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Mode is the access a mount grants inside the sandbox.
type Mode string

const (
	ReadOnly  Mode = "ro"
	ReadWrite Mode = "rw"
)

// Well-known sandbox paths. These are part of the tool contract (tool-spec §8)
// and must not be renumbered without a spec change.
const (
	PathWorkspace = "/workspace" // data/ws/<user>/<tool>          rw, builtin
	PathHome      = "/home"      // parent of the virtual home
	PathJobDir    = "/workspace/jobs"
	PathTool      = "/tool" // the tool package                ro, builtin
	PathFlow      = "/flow" // the user's flow runs (../flow.PathFlow)  rw, builtin
	PathTmp       = "/tmp"  // tmpfs                           rw, builtin
)

// HomePath returns the virtual home's sandbox path for a registered user.
func HomePath(user string) string { return "/home/" + user }

// JobRootPath returns the sandbox path of one job's directory, which is also
// the task's working directory.
func JobRootPath(jobID string) string { return PathJobDir + "/" + jobID }

// Mount is one entry of the declarative mount table.
type Mount struct {
	HostPath    string // canonical host path
	SandboxPath string // absolute path inside the sandbox
	Mode        Mode
	// Origin records why this mount exists, for audit output and error
	// messages: "builtin" | "tool" | "env" | "storage:<id>".
	Origin string
}

// Spec is a validated set of mounts.
type Spec struct {
	mounts []Mount // sorted by SandboxPath length desc, then lexicographically
}

// Add inserts a mount, rejecting duplicates and overlapping sandbox paths.
//
// Overlap is rejected rather than resolved because a nested mount is exactly
// the mistake that widens the data a sandbox can reach: the caller should have
// declared only the narrower path.
func (s *Spec) Add(m Mount) error {
	if m.HostPath == "" {
		return fmt.Errorf("mount for %s: empty host path", m.SandboxPath)
	}
	if !strings.HasPrefix(m.SandboxPath, "/") {
		return fmt.Errorf("mount %s: sandbox path must be absolute", m.SandboxPath)
	}
	if m.Mode != ReadOnly && m.Mode != ReadWrite {
		return fmt.Errorf("mount %s: mode must be ro|rw, got %q", m.SandboxPath, m.Mode)
	}
	m.HostPath = filepath.Clean(m.HostPath)
	m.SandboxPath = cleanSlash(m.SandboxPath)
	for _, existing := range s.mounts {
		if existing.SandboxPath == m.SandboxPath {
			return fmt.Errorf("duplicate sandbox path %s (%s and %s)", m.SandboxPath, existing.Origin, m.Origin)
		}
		if isUnder(m.SandboxPath, existing.SandboxPath) || isUnder(existing.SandboxPath, m.SandboxPath) {
			return fmt.Errorf("sandbox paths overlap: %s (%s) and %s (%s) — bind the exact paths you need, never a parent",
				existing.SandboxPath, existing.Origin, m.SandboxPath, m.Origin)
		}
	}
	s.mounts = append(s.mounts, m)
	sort.Slice(s.mounts, func(i, j int) bool {
		if len(s.mounts[i].SandboxPath) != len(s.mounts[j].SandboxPath) {
			return len(s.mounts[i].SandboxPath) > len(s.mounts[j].SandboxPath)
		}
		return s.mounts[i].SandboxPath < s.mounts[j].SandboxPath
	})
	return nil
}

// MustAdd is Add for builtin mounts whose parameters are constants.
func (s *Spec) MustAdd(m Mount) {
	if err := s.Add(m); err != nil {
		panic(err)
	}
}

// Mounts returns the mounts in resolution order (longest sandbox path first).
func (s *Spec) Mounts() []Mount { return append([]Mount(nil), s.mounts...) }

// Resolve maps a sandbox path to its host path and owning mount.
//
// It is purely structural: the returned host path is not guaranteed to exist.
// A sandbox path outside every mount is an error — that is the whole point.
func (s *Spec) Resolve(sandboxPath string) (string, Mount, error) {
	p := cleanSlash(sandboxPath)
	for _, m := range s.mounts {
		if p == m.SandboxPath {
			return m.HostPath, m, nil
		}
		if strings.HasPrefix(p, m.SandboxPath+"/") {
			rest := strings.TrimPrefix(p, m.SandboxPath+"/")
			candidate := filepath.Join(m.HostPath, filepath.FromSlash(rest))
			// Re-check after Join: a `..` segment would have escaped.
			if !isUnderFS(candidate, m.HostPath) {
				return "", m, fmt.Errorf("path escapes mount %s: %s", m.SandboxPath, sandboxPath)
			}
			return candidate, m, nil
		}
	}
	return "", Mount{}, fmt.Errorf("sandbox path %q is not under any mount", sandboxPath)
}

// ResolveExisting is Resolve with symlink safety: the deepest existing
// ancestor is canonicalized and re-checked, so a symlink planted inside a
// writable mount cannot redirect the lookup outside it.
func (s *Spec) ResolveExisting(sandboxPath string) (string, error) {
	host, m, err := s.Resolve(sandboxPath)
	if err != nil {
		return "", err
	}
	existing, err := deepestExisting(host)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	if !isUnderFS(canonical, m.HostPath) {
		return "", fmt.Errorf("symlink escapes mount %s: %s", m.SandboxPath, sandboxPath)
	}
	// Rebuild the tail that did not exist yet on top of the canonical prefix.
	if existing != host {
		tail, err := filepath.Rel(existing, host)
		if err != nil {
			return "", err
		}
		canonical = filepath.Join(canonical, tail)
	}
	return canonical, nil
}

// Display is the inverse of Resolve: host path -> sandbox path. Used when
// SRCOS reports a location back to a user or hands a path to a tool.
func (s *Spec) Display(hostPath string) (string, error) {
	p := filepath.Clean(hostPath)
	for _, m := range s.mounts {
		if !isUnderFS(p, m.HostPath) {
			continue
		}
		rel, err := filepath.Rel(m.HostPath, p)
		if err != nil {
			continue
		}
		if rel == "." {
			return m.SandboxPath, nil
		}
		return m.SandboxPath + "/" + filepath.ToSlash(rel), nil
	}
	return "", fmt.Errorf("host path %q is not visible in the sandbox", hostPath)
}

// FindByOrigin returns the first mount with the given origin prefix.
func (s *Spec) FindByOrigin(origin string) (Mount, bool) {
	for _, m := range s.mounts {
		if m.Origin == origin {
			return m, true
		}
	}
	return Mount{}, false
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

func cleanSlash(p string) string {
	c := filepath.ToSlash(filepath.Clean(p))
	if !strings.HasPrefix(c, "/") {
		c = "/" + c
	}
	return c
}

// isUnder reports whether child is strictly below parent (segment boundary).
func isUnder(child, parent string) bool {
	return strings.HasPrefix(child, parent+"/")
}

// isUnderFS is the filesystem flavour of isUnder, tolerant of a trailing
// separator in parent.
func isUnderFS(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func deepestExisting(p string) (string, error) {
	cur := p
	for {
		if _, err := os.Lstat(cur); err == nil {
			return cur, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no existing ancestor for %s", p)
		}
		cur = parent
	}
}
