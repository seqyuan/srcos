// Package inspect is the read-only view of the platform for one user.
//
// It exists because the same facts have two front-ends (ADR-018): the REST API
// that the generated form and the path picker call, and the MCP server that an
// agent calls. Both must answer "which tools may this user see", "what is in
// this storage", "what happened to this instance" identically — a second
// implementation would drift, and the drift would be a security bug (one
// front-end filtering grants and the other not).
//
// Nothing here writes, starts or stops anything. The write path (submit a job,
// start a service) lives in package api, deliberately: ADR-019 keeps that
// surface closed until the second phase.
//
// Two spellings are kept apart on purpose:
//
//   - **sandbox paths** (`/workspace/out`, `/data/ref`) are the contract: a
//     tool receives them, a user picks them, an agent echoes them back;
//   - **host paths** are SRCOS's business and never leave this package. Every
//     lookup goes through sandbox.Jail (AGENTS.md: Jail is the only path
//     resolution entry point), so `..` and symlink escapes are refused here
//     rather than at each front-end.
package inspect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/resource"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// Error classes. The front-ends map these onto their own vocabulary (HTTP
// status codes, MCP tool errors); the classification itself is the read side's
// decision so the two cannot disagree about what "not found" means.
var (
	// ErrNotFound: the thing exists nowhere, or the caller may not know that it
	// does. "Unknown" and "not authorized" are deliberately one class for
	// tools: telling a caller which tools exist but are not theirs is a leak.
	ErrNotFound = errors.New("not found")
	// ErrForbidden: the caller is known and this is refused (a storage the tool
	// did not declare, a path outside every mount).
	ErrForbidden = errors.New("not permitted")
	// ErrBadRequest: the request itself is wrong (missing argument, malformed
	// path, a file that is not text).
	ErrBadRequest = errors.New("invalid request")
	// ErrUnavailable: this deployment has no such subsystem configured (no tool
	// directory, no storage provider, no instance store).
	ErrUnavailable = errors.New("not configured on this host")
)

// Grants answers "may this user see and use this tool".
//
// One method, because the read side only ever asks that: quotas and admin
// status are properties of the *write* path and live where they are enforced.
// A richer interface here would invite a second implementation of them.
type Grants interface {
	Allowed(username, toolID string) bool
}

// Reader answers the read-only questions for a deployment.
//
// It holds no state of its own: every call reads the filesystem, which is where
// the truth lives (AGENTS.md: 文件系统即数据库). That also means a caller never
// has to invalidate a cache to see a new tool or a finished job.
type Reader struct {
	// ConfigDir holds the runtime state (data/instances, data/ws, ...).
	ConfigDir string
	// ToolsDir is the tool package root. Empty disables every tool answer.
	ToolsDir string
	// Storages is the StorageProvider. Nil means this host declares no shared
	// data.
	Storages storage.Provider
	// Grants filters the catalogue. Nil means authorization is not wired (a
	// single-user deployment) and every tool is visible.
	Grants Grants
	// Viewers decides which viewer claims a srcos:// resource (ADR-016's
	// registry). Nil uses the built-in first batch.
	Viewers *resource.Registry
	// FollowInterval overrides how often FollowLogs polls for new output. Zero
	// uses the package default; it exists so tests do not wait a second per
	// observation.
	FollowInterval time.Duration
}

// StorageView is one declared data root, as a caller sees it.
type StorageView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Root is the *sandbox* path — the value a tool receives. The host root is
	// not part of any answer.
	Root string `json:"root"`
	Mode string `json:"mode"`
	// Tools lists the tools that declare this storage, so a reader can tell
	// which tool a path is usable with (ADR-020's closure, made visible).
	Tools []string `json:"tools,omitempty"`
}

// ToolView is a tool as the read side presents it: the contract, never the host
// directory the package lives at.
type ToolView struct {
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Kind        string         `json:"kind"`
	Backend     string         `json:"backend"`
	Sandbox     string         `json:"sandbox,omitempty"`
	Entry       string         `json:"entry,omitempty"`
	Interface   tool.Interface `json:"interface"`
	Resources   tool.Resources `json:"resources,omitempty"`
	Storages    []StorageView  `json:"storages,omitempty"`
}

// viewOf renders a manifest the way callers see it.
func viewOf(t *tool.Tool, storages []StorageView) ToolView {
	return ToolView{
		ID:          t.ID,
		Version:     t.Version,
		Name:        t.Name,
		Description: t.Description,
		Kind:        string(t.Kind),
		Backend:     string(t.Backend),
		Sandbox:     string(t.Sandbox),
		Entry:       t.Entry,
		Interface:   t.Interface,
		Resources:   t.Resources,
		Storages:    storages,
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 工具
// ─────────────────────────────────────────────────────────────────────────

// Tools is the catalogue this user may see, ordered by id.
func (r *Reader) Tools(username string) ([]ToolView, error) {
	manifests, err := r.VisibleManifests(username)
	if err != nil {
		return nil, err
	}
	out := make([]ToolView, 0, len(manifests))
	for _, t := range manifests {
		out = append(out, viewOf(t, r.storageViewsFor(t)))
	}
	return out, nil
}

// Tool describes one tool, with the storages its path parameters may select.
func (r *Reader) Tool(username, id string) (*ToolView, error) {
	t, err := r.VisibleManifest(username, id)
	if err != nil {
		return nil, err
	}
	v := viewOf(t, r.storageViewsFor(t))
	return &v, nil
}

// Manifest returns a tool's manifest by id, *without* a grant check.
//
// It exists for the instance-scoped answers: a user's own outputs and logs stay
// readable after a tool is no longer granted to them, because the record is
// theirs. Discovery ("what may I run") always goes through Tools/Tool instead.
func (r *Reader) Manifest(id string) (*tool.Tool, error) {
	if r.ToolsDir == "" {
		return nil, fmt.Errorf("%w: no tool directory is configured", ErrUnavailable)
	}
	t, err := tool.Find(r.ToolsDir, id)
	if err != nil {
		return nil, fmt.Errorf("%w: tool %s", ErrNotFound, id)
	}
	return t, nil
}

// VisibleManifest looks up one tool this user may see.
//
// It is the choke point for "may this user know that this tool exists": the
// two ways it can fail — unknown, or known-but-not-granted — return the same
// class on purpose, because telling a caller which tools exist but are not
// theirs is itself a disclosure.
func (r *Reader) VisibleManifest(username, id string) (*tool.Tool, error) {
	if r.ToolsDir == "" {
		return nil, fmt.Errorf("%w: no tool directory is configured", ErrUnavailable)
	}
	manifests, err := r.VisibleManifests(username)
	if err != nil {
		return nil, err
	}
	for _, t := range manifests {
		if t.ID == id {
			return t, nil
		}
	}
	if t, err := tool.Find(r.ToolsDir, id); err == nil && t != nil {
		return nil, fmt.Errorf("%w: tool %s is not authorized for you", ErrNotFound, id)
	}
	return nil, fmt.Errorf("%w: unknown tool %s", ErrNotFound, id)
}

// VisibleManifests lists the tools this user may see, ordered by id.
//
// Exported for the front-ends that render a manifest rather than a view (the
// HTML tool pages and the generated form). The decision itself stays here, so
// all of them ask the same question.
func (r *Reader) VisibleManifests(username string) ([]*tool.Tool, error) {
	if r.ToolsDir == "" {
		return nil, nil
	}
	all, err := tool.Discover(r.ToolsDir)
	if err != nil {
		// A missing directory is the same statement as an empty one — "this
		// deployment has no tools (yet)" — and the startup path already reads it
		// that way. A *malformed* tool still surfaces, because that is a
		// configuration mistake rather than a fresh install.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if r.Grants == nil {
		return all, nil
	}
	allowed := make([]*tool.Tool, 0, len(all))
	for _, t := range all {
		if r.Grants.Allowed(username, t.ID) {
			allowed = append(allowed, t)
		}
	}
	return allowed, nil
}

// ─────────────────────────────────────────────────────────────────────────
// 存储
// ─────────────────────────────────────────────────────────────────────────

// StoragesForUser lists every data root the user's visible tools declare.
//
// Not "every storage in storages.yaml": the selection range of a path
// parameter is the set of storages the tool declares (ADR-020), so listing a
// root no tool of this user mentions would offer something they cannot then
// use — and would disclose the shape of other projects' data.
func (r *Reader) StoragesForUser(username string) ([]StorageView, error) {
	if r.Storages == nil {
		return []StorageView{}, nil
	}
	manifests, err := r.VisibleManifests(username)
	if err != nil {
		return nil, err
	}

	byID := map[string]*StorageView{}
	for _, t := range manifests {
		for _, id := range t.RequiresStorages {
			s, ok := r.Storages.Get(id)
			if !ok {
				continue
			}
			v, seen := byID[id]
			if !seen {
				v = &StorageView{ID: s.ID, Name: s.Name, Root: s.SandboxPath, Mode: string(s.Mode)}
				byID[id] = v
			}
			v.Tools = append(v.Tools, t.ID)
		}
	}

	out := make([]StorageView, 0, len(byID))
	for _, v := range byID {
		sort.Strings(v.Tools)
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// StorageDefs returns a tool's declared storages as definitions rather than
// views.
//
// The generated form (and the srcos-path-picker it embeds) needs the host-root
// definition to render a picker; the view deliberately hides those. It is the
// only other shape storage leaves this package in, and it goes to a renderer
// running in the same trust domain (the gateway), never to a caller.
func (r *Reader) StorageDefs(t *tool.Tool) []storage.Storage {
	if r.Storages == nil || len(t.RequiresStorages) == 0 {
		return nil
	}
	out := make([]storage.Storage, 0, len(t.RequiresStorages))
	for _, id := range t.RequiresStorages {
		if s, ok := r.Storages.Get(id); ok {
			out = append(out, s)
		}
	}
	return out
}

// storageViewsFor renders the storages a tool declares, in declaration order.
func (r *Reader) storageViewsFor(t *tool.Tool) []StorageView {
	if r.Storages == nil || len(t.RequiresStorages) == 0 {
		return nil
	}
	out := make([]StorageView, 0, len(t.RequiresStorages))
	for _, id := range t.RequiresStorages {
		s, ok := r.Storages.Get(id)
		if !ok {
			continue
		}
		out = append(out, StorageView{ID: s.ID, Name: s.Name, Root: s.SandboxPath, Mode: string(s.Mode)})
	}
	return out
}

// storageViews renders a specific id list (used by the path browser).
func (r *Reader) storageViews(ids []string) []StorageView {
	if r.Storages == nil {
		return nil
	}
	out := make([]StorageView, 0, len(ids))
	for _, id := range ids {
		s, ok := r.Storages.Get(id)
		if !ok {
			continue
		}
		out = append(out, StorageView{ID: s.ID, Name: s.Name, Root: s.SandboxPath, Mode: string(s.Mode)})
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// 路径浏览
// ─────────────────────────────────────────────────────────────────────────

// PathRequest is one directory-listing request.
type PathRequest struct {
	Tool  string
	Input string
	// Storage narrows the choice when one input declares several roots. It can
	// only ever select from the input's own list.
	Storage string
	// Path is a sandbox path. Empty means the chosen storage's root.
	Path   string
	Select string
	Limit  int
}

// PathListing is what the picker (and the MCP tool) renders from.
type PathListing struct {
	Tool     string        `json:"tool"`
	Input    string        `json:"input"`
	Select   string        `json:"select,omitempty"`
	Storages []StorageView `json:"storages"`
	Storage  string        `json:"storage"`
	Path     string        `json:"path"`
	// Parent is the enclosing directory, for an "up" control. Empty at a root.
	Parent    string          `json:"parent,omitempty"`
	Entries   []storage.Entry `json:"entries"`
	Truncated bool            `json:"truncated,omitempty"`
}

// Paths lists one directory inside a storage, as a sandbox path.
//
// The storage is derived from the tool and input the caller names, never taken
// from a free-form id: that is ADR-020's closure enforced at the read edge, so
// "what a picker offers" and "what a sandbox mounts" cannot drift.
func (r *Reader) Paths(ctx context.Context, username string, req PathRequest) (*PathListing, error) {
	if r.Storages == nil {
		return nil, fmt.Errorf("%w: no storage provider is configured", ErrUnavailable)
	}
	if strings.TrimSpace(req.Tool) == "" || strings.TrimSpace(req.Input) == "" {
		return nil, fmt.Errorf("%w: both `tool` and `input` are required", ErrBadRequest)
	}

	t, err := r.VisibleManifest(username, req.Tool)
	if err != nil {
		return nil, err
	}
	in, ok := findInput(t.Interface, req.Input)
	if !ok {
		return nil, fmt.Errorf("%w: tool %s has no input %q", ErrNotFound, t.ID, req.Input)
	}
	ids := fromIDs(in.From)
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: input %s is not storage-bound: only type path (or file/directory with a `from`) can be browsed", ErrBadRequest, req.Input)
	}

	// The input's own declaration is the allowlist.
	chosen := strings.TrimSpace(req.Storage)
	if chosen == "" {
		chosen = ids[0]
	}
	if !contains(ids, chosen) {
		return nil, fmt.Errorf("%w: storage %q is not declared by tool %s input %s (allowed: %v)",
			ErrForbidden, chosen, t.ID, req.Input, ids)
	}

	sel := storage.Select(strings.TrimSpace(req.Select))
	if sel == "" {
		sel = selectOf(in)
	}

	views := r.storageViews(ids)
	sandboxPath := strings.TrimSpace(req.Path)
	if sandboxPath == "" {
		for _, s := range views {
			if s.ID == chosen {
				sandboxPath = s.Root
			}
		}
	}

	listing, err := r.Storages.Listing(ctx, chosen, sandboxPath, sel, req.Limit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}

	res := &PathListing{
		Tool:     t.ID,
		Input:    req.Input,
		Select:   string(sel),
		Storage:  chosen,
		Storages: views,
		Path:     listing.Path,
		Entries:  listing.Entries,
	}
	if res.Entries == nil {
		res.Entries = []storage.Entry{}
	}
	res.Truncated = listing.Truncated
	if parent := parentOf(listing.Path); parent != "" {
		if _, err := r.Storages.Resolve(chosen, parent); err == nil {
			res.Parent = parent
		}
	}
	return res, nil
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

func findInput(i tool.Interface, name string) (tool.Input, bool) {
	for _, in := range i.Inputs {
		if in.Name == name {
			return in, true
		}
	}
	return tool.Input{}, false
}

// fromIDs splits an input's `from` field into storage ids.
func fromIDs(from string) []string {
	var out []string
	for _, p := range strings.Split(from, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// selectOf maps an input's type and select onto a listing filter, so a
// directory-typed parameter can never be handed a file.
func selectOf(in tool.Input) storage.Select {
	switch in.Type {
	case tool.TypeFile:
		return storage.SelectFile
	case tool.TypeDirectory, tool.TypeDirPath:
		return storage.SelectDirectory
	case tool.TypePath:
		switch in.Select {
		case "file":
			return storage.SelectFile
		case "directory":
			return storage.SelectDirectory
		}
	}
	return storage.SelectAny
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// parentOf returns the enclosing sandbox path, or "" at a mount root.
func parentOf(p string) string {
	p = strings.TrimSuffix(p, "/")
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// under reports whether child is inside parent (sandbox paths, both absolute).
func under(child, parent string) bool {
	child = strings.TrimSuffix(child, "/")
	parent = strings.TrimSuffix(parent, "/")
	if parent == "" {
		return false
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}

// homeDir is the user's virtual home on the host (ADR-021).
func (r *Reader) homeDir(username string) string {
	return config.HomeDir(r.ConfigDir, username)
}
