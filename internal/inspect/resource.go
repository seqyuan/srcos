package inspect

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/resource"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
)

// This file is the read side of the srcos:// protocol (ADR-011/016): a parsed
// address becomes a host path here and nowhere else, behind the same jail the
// sandbox uses. The address grammar lives in package resource (pure parsing);
// this is the half that touches the filesystem.
//
// Two properties are worth stating because every front-end depends on them:
//
//   - **An address is user-relative.** There is no username component, so a
//     caller cannot name another user's home; the scope is resolved as the
//     requesting user (ADR-021).
//   - **The scopes are the visible ones.** `workspace` requires a tool the user
//     is granted, and a storage scope must appear in the closure of the user's
//     visible tools (ADR-020) — the same rule `/api/paths` enforces, so "what a
//     viewer can open" and "what a sandbox mounts" cannot drift.

// ResourceRequest is a parsed address's resolution half.
type ResourceRequest struct {
	Scope string
	// Path is inside the scope, without a leading slash ("" is the root).
	Path string
	// Tool narrows scope=workspace.
	Tool string
}

// ResourceEntry is one directory entry of a resource scope.
type ResourceEntry struct {
	Name string `json:"name"`
	// Path is the sandbox path (the contract spelling).
	Path string `json:"path"`
	// Rel is the path inside the scope — what an address is built from.
	Rel   string `json:"rel"`
	IsDir bool   `json:"isDir"`
	// Addr is the entry's own srcos:// address, so a client (a viewer, an agent,
	// a script) never has to compose one from scope + rel and guess the rules.
	Addr    string    `json:"addr"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime,omitempty"`
}

// ResourceView is the read side's answer for one address.
//
// It carries no host path: the caller gets the sandbox spelling, which is the
// contract (ADR-020), and the text payload only for the viewers that render
// server-side. The JSON API omits Text; the viewer page uses it.
type ResourceView struct {
	Scope       string          `json:"scope"`
	Tool        string          `json:"tool,omitempty"`
	SandboxPath string          `json:"sandboxPath"`
	IsDir       bool            `json:"isDir"`
	Size        int64           `json:"size"`
	ModTime     time.Time       `json:"mtime,omitempty"`
	Mode        string          `json:"mode,omitempty"`
	Viewer      resource.Viewer `json:"viewer"`
	// Text is the bounded content for text/markdown/table viewers.
	Text      string `json:"text,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Binary reports bytes that are not text: the text viewer says so instead
	// of rendering mojibake.
	Binary           bool            `json:"binary,omitempty"`
	Entries          []ResourceEntry `json:"entries,omitempty"`
	EntriesTruncated bool            `json:"entriesTruncated,omitempty"`
}

// ResourceScope is one root an address may name, for the landing page that lets
// a user browse without knowing an address by heart.
type ResourceScope struct {
	// Scope is the address scope segment (home, workspace, or a storage id).
	Scope string `json:"scope"`
	// Name is a human title.
	Name string `json:"name"`
	// Kind is "home" | "workspace" | "storage".
	Kind string `json:"kind"`
	// Tool is set for a workspace scope.
	Tool string `json:"tool,omitempty"`
	// Mode is ro|rw for storages and workspaces.
	Mode string `json:"mode,omitempty"`
}

// ResourceScopes lists the roots this user may browse, in a stable order:
// home, then workspaces, then storages.
func (r *Reader) ResourceScopes(username string) ([]ResourceScope, error) {
	out := []ResourceScope{{Scope: resource.ScopeHome, Name: "我的 home", Kind: "home", Mode: "rw"}}

	manifests, err := r.VisibleManifests(username)
	if err != nil {
		return nil, err
	}
	for _, t := range manifests {
		name := t.Name
		if name == "" {
			name = t.ID
		}
		out = append(out, ResourceScope{
			Scope: resource.ScopeWorkspace,
			Name:  "工作区 · " + name,
			Kind:  "workspace",
			Tool:  t.ID,
			Mode:  "rw",
		})
	}

	storages, err := r.StoragesForUser(username)
	if err != nil {
		return nil, err
	}
	for _, s := range storages {
		out = append(out, ResourceScope{
			Scope: s.ID,
			Name:  s.Name,
			Kind:  "storage",
			Mode:  s.Mode,
		})
	}
	return out, nil
}

// ResolveResource turns an address's request into a concrete location.
//
// The returned view never contains a host path; callers that need the bytes ask
// OpenResource, so the jail stays the only mapper.
func (r *Reader) ResolveResource(username string, req ResourceRequest) (*ResourceView, error) {
	spec, root, mode, err := r.resourceSpec(username, req)
	if err != nil {
		return nil, err
	}
	rel := strings.Trim(strings.TrimSpace(req.Path), "/")
	sandboxPath := root
	if rel != "" {
		sandboxPath = root + "/" + filepath.ToSlash(rel)
	}
	host, err := resolveHost(spec, sandboxPath)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(host)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, sandboxPath)
		}
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}

	return &ResourceView{
		Scope:       req.Scope,
		Tool:        req.Tool,
		SandboxPath: sandboxPath,
		IsDir:       fi.IsDir(),
		Size:        fi.Size(),
		ModTime:     fi.ModTime(),
		Mode:        mode,
	}, nil
}

// resourceSpec builds the mount table one address scope may see, and reports
// the scope's sandbox root and access mode.
//
// Only the requested scope is mounted: a viewer for `/workspace` has no reason
// to also see the home or any storage, and building the narrow table is what
// makes the jail's answer meaningful.
func (r *Reader) resourceSpec(username string, req ResourceRequest) (*sandbox.Spec, string, string, error) {
	spec := &sandbox.Spec{}
	switch req.Scope {
	case resource.ScopeHome:
		home := config.HomeDir(r.ConfigDir, username)
		if err := spec.Add(sandbox.Mount{
			HostPath:    home,
			SandboxPath: sandbox.HomePath(username),
			Mode:        sandbox.ReadWrite,
			Origin:      "builtin",
		}); err != nil {
			return nil, "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return spec, sandbox.HomePath(username), "rw", nil

	case resource.ScopeWorkspace:
		if strings.TrimSpace(req.Tool) == "" {
			return nil, "", "", fmt.Errorf("%w: scope workspace requires a tool", ErrBadRequest)
		}
		t, err := r.VisibleManifest(username, req.Tool)
		if err != nil {
			return nil, "", "", err
		}
		if err := spec.Add(sandbox.Mount{
			HostPath:    config.WorkspaceDir(r.ConfigDir, username, t.ID),
			SandboxPath: sandbox.PathWorkspace,
			Mode:        sandbox.ReadWrite,
			Origin:      "builtin",
		}); err != nil {
			return nil, "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return spec, sandbox.PathWorkspace, "rw", nil

	default:
		if r.Storages == nil {
			return nil, "", "", fmt.Errorf("%w: no storage provider is configured", ErrUnavailable)
		}
		// The closure check: only storages the user's visible tools declare.
		visible, err := r.visibleStorages(username)
		if err != nil {
			return nil, "", "", err
		}
		for _, s := range visible {
			if s.ID != req.Scope {
				continue
			}
			if err := spec.Add(sandbox.Mount{
				HostPath:    s.HostRoot,
				SandboxPath: s.SandboxPath,
				Mode:        sandbox.Mode(s.Mode),
				Origin:      storage.OriginFor(s.ID),
			}); err != nil {
				return nil, "", "", fmt.Errorf("%w: %v", ErrUnavailable, err)
			}
			return spec, s.SandboxPath, string(s.Mode), nil
		}
		// Unknown and not-declared are one class on purpose: telling a caller
		// which storages exist but are not theirs is a disclosure (same rule as
		// tools).
		if _, ok := r.Storages.Get(req.Scope); ok {
			return nil, "", "", fmt.Errorf("%w: storage %q is not declared by any tool of yours", ErrForbidden, req.Scope)
		}
		return nil, "", "", fmt.Errorf("%w: unknown storage %q", ErrNotFound, req.Scope)
	}
}

// OpenResource opens the file an address names, after the same resolution.
//
// The caller streams and closes it. Only the file is returned: an *os.File
// carries its base name and no directory, so a host path cannot leak through
// the answer.
func (r *Reader) OpenResource(username string, req ResourceRequest) (*os.File, error) {
	view, err := r.ResolveResource(username, req)
	if err != nil {
		return nil, err
	}
	if view.IsDir {
		return nil, fmt.Errorf("%w: %s is a directory", ErrBadRequest, view.SandboxPath)
	}
	// Re-resolve to a host path; ResolveResource deliberately does not return
	// one.
	spec, root, _, err := r.resourceSpec(username, req)
	if err != nil {
		return nil, err
	}
	rel := strings.Trim(strings.TrimSpace(req.Path), "/")
	sandboxPath := root
	if rel != "" {
		sandboxPath = root + "/" + filepath.ToSlash(rel)
	}
	host, err := resolveHost(spec, sandboxPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(host)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, view.SandboxPath)
		}
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	return f, nil
}

// resolveHost maps a sandbox path to a host path through the jail.
//
// The existence check comes first, on purpose: ResolveExisting validates a
// symlink by canonicalizing the deepest *existing* ancestor, so for a path that
// does not exist yet that ancestor necessarily sits outside the mount and the
// lookup reports a missing file as a "symlink escape". Asking Lstat first turns
// that into the honest ErrNotFound, and leaves ResolveExisting to do the job it
// was written for.
func resolveHost(spec *sandbox.Spec, sandboxPath string) (string, error) {
	host, _, err := spec.Resolve(sandboxPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	if _, err := os.Lstat(host); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, sandboxPath)
		}
		return "", fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	canonical, err := spec.ResolveExisting(sandboxPath)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	return canonical, nil
}

// DescribeResource resolves an address, decides which viewer claims it, and (for
// a directory) lists it — everything except the file's bytes.
//
// claimedBy overrides the registry's decision when non-empty — an explicit
// "open as" from a user who knows better than the extension.
func (r *Reader) DescribeResource(username string, req ResourceRequest, claimedBy string) (*ResourceView, error) {
	view, err := r.ResolveResource(username, req)
	if err != nil {
		return nil, err
	}

	registry := r.Viewers
	if registry == nil {
		registry = resource.Default()
	}
	viewer := registry.Claim(req.Path, view.IsDir)
	if id := strings.TrimSpace(claimedBy); id != "" {
		if v, ok := registry.Lookup(id); ok {
			viewer = v
		} else {
			return nil, fmt.Errorf("%w: unknown viewer %q", ErrBadRequest, id)
		}
	}
	// The filesystem wins over an override: a directory has no bytes to show as
	// a text file, however the user asked.
	if view.IsDir {
		if v, ok := registry.Lookup("dir"); ok {
			viewer = v
		}
	}
	view.Viewer = viewer

	if view.IsDir {
		entries, truncated, err := r.listResourceDir(username, req)
		if err != nil {
			return nil, err
		}
		view.Entries, view.EntriesTruncated = entries, truncated
	}
	return view, nil
}

// ViewResource is the full answer a viewer page renders from: the resolved
// location, which viewer claims it, and (for the server-rendered viewers) the
// bounded payload.
func (r *Reader) ViewResource(username string, req ResourceRequest, claimedBy string) (*ResourceView, error) {
	view, err := r.DescribeResource(username, req, claimedBy)
	if err != nil {
		return nil, err
	}
	switch view.Viewer.Kind {
	case resource.KindText, resource.KindMarkdown, resource.KindTable:
		text, truncated, binary, err := r.readResourceText(username, req)
		if err != nil {
			return nil, err
		}
		view.Text, view.Truncated, view.Binary = text, truncated, binary
	}
	return view, nil
}

// maxResourceLines bounds what a listing returns (a directory with a million
// entries must not become a page).
const maxResourceEntries = 1000

// listResourceDir lists the directory an address names, in the sandbox's path
// space.
func (r *Reader) listResourceDir(username string, req ResourceRequest) ([]ResourceEntry, bool, error) {
	spec, root, _, err := r.resourceSpec(username, req)
	if err != nil {
		return nil, false, err
	}
	rel := strings.Trim(strings.TrimSpace(req.Path), "/")
	sandboxPath := root
	if rel != "" {
		sandboxPath = root + "/" + filepath.ToSlash(rel)
	}
	host, err := resolveHost(spec, sandboxPath)
	if err != nil {
		return nil, false, err
	}
	dir, err := os.Open(host)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	defer dir.Close()

	raw, err := dir.ReadDir(maxResourceEntries + 1)
	if err != nil && err != io.EOF {
		return nil, false, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	truncated := len(raw) > maxResourceEntries
	if truncated {
		raw = raw[:maxResourceEntries]
	}

	entries := make([]ResourceEntry, 0, len(raw))
	for _, e := range raw {
		isDir := e.IsDir()
		// A symlink is reported as its target's kind, so the UI never offers a
		// "directory" the jail will then refuse.
		if e.Type()&os.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(host, e.Name()))
			if err != nil {
				continue // dangling or escaping: not listable
			}
			isDir = info.IsDir()
		}
		entryRel := joinSandboxPath(rel, e.Name())
		entry := ResourceEntry{
			Name:  e.Name(),
			Path:  joinSandboxPath(sandboxPath, e.Name()),
			Rel:   entryRel,
			IsDir: isDir,
			// The request already names the scope (and the tool), so the child
			// address is a construction, not a lookup.
			Addr: resource.Address{
				Provider: resource.ProviderFile,
				Scope:    req.Scope,
				Path:     entryRel,
				Tool:     req.Tool,
			}.String(),
		}
		if info, err := e.Info(); err == nil {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime()
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir // directories first
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, truncated, nil
}

// readResourceText reads a bounded, validated text payload for the viewers that
// render server-side.
func (r *Reader) readResourceText(username string, req ResourceRequest) (text string, truncated, binary bool, err error) {
	f, err := r.OpenResource(username, req)
	if err != nil {
		return "", false, false, err
	}
	defer f.Close()

	max := int64(MaxReadBytes)
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", false, false, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	if int64(len(data)) > max {
		truncated = true
		data = data[:max]
	}
	// The byte cap can cut a multi-byte rune in half; drop the partial tail
	// before judging validity, so a truncation is not reported as binary.
	data = validPrefix(data)
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", truncated, true, nil
	}
	return string(data), truncated, false, nil
}

// joinSandboxPath joins a sandbox path with a name ("" base means the root).
func joinSandboxPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}
