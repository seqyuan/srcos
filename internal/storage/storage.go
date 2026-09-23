// Package storage is the StorageProvider: the declarative list of host data
// roots a sandbox may see, plus the jail-checked lookup behind /api/paths and
// the MCP srcos_list_paths tool.
//
// This is where AGENTS.md's "环境与数据要分开" rule is enforced:
//
//   - *Environment* (conda trees, module prefixes, .sif overlays, a tool's own
//     binaries) is declared by the tool author in tool.yaml's ro_mounts and is
//     allowed to be coarse, because it holds no user data.
//   - *Data* (shared reference sets, project directories) is declared here by
//     the administrator and is the only thing a `type: path` parameter may
//     select from.
//
// The closure that makes this safe (ADR-020):
//
//	a path parameter's range == the storages mounted into the instance
//	                        == the tool's requires_storages
//
// so a user can never pick something the sandbox cannot see, and cannot reach
// anything the tool did not ask for.
//
// Granularity is expressed by declaring narrower storages, not by adding a
// sub-path option: because bubblewrap's user namespace collapses every unmapped
// gid to 65534 (and the sandboxed process is in group 65534), the host `group`
// permission bit is effectively public inside a sandbox. A storage whose
// HostRoot is a parent directory therefore hands over everything under it that
// is group-readable. Declare the narrowest root that works.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/resource"
	"github.com/seqyuan/srcos/internal/sandbox"
)

// Kind is how the data is reached. Only posix is implemented; s3 is a reserved
// field so the schema does not have to change when it lands.
type Kind string

const (
	KindPosix Kind = "posix"
	KindS3    Kind = "s3"
)

// Mode is the access a storage grants.
type Mode string

const (
	ReadOnly  Mode = "ro"
	ReadWrite Mode = "rw"
)

// Select filters which entry kinds a listing returns. It mirrors the
// `select:` field of a `type: path` input, so a directory-typed parameter can
// never be handed a file.
type Select string

const (
	SelectAny       Select = ""
	SelectFile      Select = "file"
	SelectDirectory Select = "directory"
)

// Storage is one declared data root.
type Storage struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Kind        Kind   `yaml:"type"`
	HostRoot    string `yaml:"host_root"`
	SandboxPath string `yaml:"sandbox_path"`
	Mode        Mode   `yaml:"mode"`
	Description string `yaml:"description,omitempty"`
}

// Entry is one directory entry, described in the sandbox's path space.
type Entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"` // sandbox path, directly usable by a tool
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size,omitempty"`
	ModTime time.Time `json:"mtime,omitempty"`
}

// Listing is one directory listing.
type Listing struct {
	Storage   string  `json:"storage"`
	Path      string  `json:"path"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
}

// ErrNotFound is returned when a storage id is unknown.
var ErrNotFound = errors.New("unknown storage")

// Provider is the read side of the storage seam. The runtime consumes it for
// mounts; the HTTP API and MCP consume it for listing.
type Provider interface {
	// List returns every declared storage, ordered by id.
	List() []Storage
	// Get returns one storage by id.
	Get(id string) (Storage, bool)
	// ForTool resolves a tool's requires_storages into concrete storages,
	// rejecting unknown ids — the registration-time cross-check of ADR-020.
	ForTool(ids []string) ([]Storage, error)
	// Listing enumerates one directory inside one storage.
	Listing(ctx context.Context, storageID, sandboxPath string, sel Select, limit int) (*Listing, error)
	// Resolve maps a sandbox path back to its host path, jail-checked.
	Resolve(storageID, sandboxPath string) (string, error)
	// Display maps a host path into the sandbox path space.
	Display(hostPath string) (string, error)
}

// ─────────────────────────────────────────────────────────────────────────
// 配置加载
// ─────────────────────────────────────────────────────────────────────────

// ConfigFile is the administrator-authored storages.yaml.
type ConfigFile struct {
	Storages []Storage `yaml:"storages"`
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Load reads and validates storages.yaml. A missing file yields an empty
// provider: a deployment with no shared data is perfectly valid, and tools
// that declare requires_storages then fail loudly at registration.
func Load(path string) (Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(nil)
		}
		return nil, err
	}
	var cf ConfigFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p, err := New(cf.Storages)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// New validates a storage list and builds the provider.
//
// Validation is strict because every mistake here is a data-exposure mistake:
// an unknown kind, a relative root, or overlapping sandbox paths are all
// rejected rather than best-effort coerced.
func New(items []Storage) (Provider, error) {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	seen := map[string]bool{}
	for i := range items {
		s := &items[i]
		where := fmt.Sprintf("storages[%d]", i)
		if s.ID != "" {
			where = fmt.Sprintf("storage %q", s.ID)
		}

		if !idRe.MatchString(s.ID) {
			bad("%s: id must match %s", where, idRe)
		}
		// The two builtin resource scopes (ADR-011/016: srcos://file/home/...).
		// Reserving them here keeps an address from being ambiguous.
		if s.ID == resource.ScopeHome || s.ID == resource.ScopeWorkspace {
			bad("%s: id %q is reserved (it names a builtin resource scope)", where, s.ID)
		}
		if seen[s.ID] {
			bad("%s: duplicate id", where)
		}
		seen[s.ID] = true

		switch s.Kind {
		case KindPosix:
		case KindS3:
			bad("%s: type s3 is reserved but not implemented yet (ADR-020: posix first)", where)
		case "":
			bad("%s: type is required", where)
		default:
			bad("%s: unknown type %q", where, s.Kind)
		}

		if !filepath.IsAbs(s.HostRoot) {
			bad("%s: host_root must be an absolute host path, got %q", where, s.HostRoot)
		}
		if !strings.HasPrefix(s.SandboxPath, "/") {
			bad("%s: sandbox_path must be absolute, got %q", where, s.SandboxPath)
		}
		if sandbox.SandboxPathIsReserved(s.SandboxPath) {
			bad("%s: sandbox_path %q is inside a read-only system directory — use %s or another fresh top-level path",
				where, s.SandboxPath, sandbox.PathToolBin)
		}
		switch s.Mode {
		case ReadOnly, ReadWrite:
		case "":
			// Defaulting to read-only is the safe direction: a storage that
			// forgot its mode should not be writable.
			s.Mode = ReadOnly
		default:
			bad("%s: mode must be ro|rw, got %q", where, s.Mode)
		}
		if s.Name == "" {
			s.Name = s.ID
		}
		s.HostRoot = filepath.Clean(s.HostRoot)
		s.SandboxPath = strings.TrimSuffix(filepath.ToSlash(filepath.Clean(s.SandboxPath)), "/")
		if s.SandboxPath == "" {
			s.SandboxPath = "/"
		}
	}

	// Overlap is rejected for the same reason MountSpec rejects it: a parent
	// storage would silently widen every mount built from the list.
	sorted := make([]Storage, len(items))
	copy(sorted, items)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SandboxPath < sorted[j].SandboxPath })
	for i := 1; i < len(sorted); i++ {
		a, b := sorted[i-1], sorted[i]
		if a.SandboxPath == b.SandboxPath {
			bad("storages %q and %q share sandbox_path %s", a.ID, b.ID, a.SandboxPath)
			continue
		}
		if strings.HasPrefix(b.SandboxPath, a.SandboxPath+"/") {
			bad("storages %q (%s) and %q (%s) overlap — declare the narrow one only",
				a.ID, a.SandboxPath, b.ID, b.SandboxPath)
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("%d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}

	p := &fileProvider{storages: sorted}
	if err := p.buildJail(); err != nil {
		return nil, err
	}
	return p, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Provider 实现
// ─────────────────────────────────────────────────────────────────────────

type fileProvider struct {
	storages []Storage
	byID     map[string]Storage
	// spec is the same Jail type the runtime uses for sandboxes, so path
	// resolution has exactly one implementation (AGENTS.md: Jail is the only
	// path-resolution entry point).
	spec *sandbox.Spec
}

func (p *fileProvider) buildJail() error {
	spec := &sandbox.Spec{}
	for _, s := range p.storages {
		mode := sandbox.ReadOnly
		if s.Mode == ReadWrite {
			mode = sandbox.ReadWrite
		}
		if err := spec.Add(sandbox.Mount{
			HostPath:    s.HostRoot,
			SandboxPath: s.SandboxPath,
			Mode:        mode,
			Origin:      OriginFor(s.ID),
		}); err != nil {
			return fmt.Errorf("storage %q: %w", s.ID, err)
		}
	}
	p.spec = spec
	p.byID = make(map[string]Storage, len(p.storages))
	for _, s := range p.storages {
		p.byID[s.ID] = s
	}
	return nil
}

// OriginFor is the MountSpec origin marker for a storage mount. Keeping the id
// in the origin is what lets the runtime report, and the provider recover,
// which storage a mount came from.
func OriginFor(id string) string { return "storage:" + id }

func originID(origin string) (string, bool) {
	id, ok := strings.CutPrefix(origin, "storage:")
	return id, ok && id != ""
}

func (p *fileProvider) List() []Storage {
	out := make([]Storage, len(p.storages))
	copy(out, p.storages)
	return out
}

func (p *fileProvider) Get(id string) (Storage, bool) {
	s, ok := p.byID[id]
	return s, ok
}

func (p *fileProvider) ForTool(ids []string) ([]Storage, error) {
	var out []Storage
	var missing []string
	for _, id := range ids {
		s, ok := p.byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		out = append(out, s)
	}
	if len(missing) > 0 {
		known := make([]string, 0, len(p.storages))
		for _, s := range p.storages {
			known = append(known, s.ID)
		}
		sort.Strings(known)
		return nil, fmt.Errorf("%w: %s (declared: %v)",
			ErrNotFound, strings.Join(missing, ", "), known)
	}
	return out, nil
}

func (p *fileProvider) Resolve(storageID, sandboxPath string) (string, error) {
	if _, ok := p.byID[storageID]; !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, storageID)
	}
	// The structural resolve gives the owning mount (so a cross-storage lookup
	// is refused), then ResolveExisting canonicalizes symlinks and re-checks, so
	// a link planted inside a writable storage cannot redirect outside it.
	_, mount, err := p.spec.Resolve(sandboxPath)
	if err != nil {
		return "", err
	}
	id, ok := originID(mount.Origin)
	if !ok || id != storageID {
		return "", fmt.Errorf("path %q does not belong to storage %q", sandboxPath, storageID)
	}
	resolved, err := p.spec.ResolveExisting(sandboxPath)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func (p *fileProvider) Display(hostPath string) (string, error) {
	return p.spec.Display(hostPath)
}

// DefaultListLimit caps a listing when the caller does not ask for a size.
const DefaultListLimit = 500

func (p *fileProvider) Listing(ctx context.Context, storageID, sandboxPath string, sel Select, limit int) (*Listing, error) {
	if limit <= 0 || limit > 5000 {
		limit = DefaultListLimit
	}
	if sandboxPath == "" {
		s, ok := p.byID[storageID]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, storageID)
		}
		sandboxPath = s.SandboxPath
	}
	// Listing is a read; a caller must not be able to enumerate a path the
	// tool could not mount, so Resolve (which carries the jail check) is the
	// gate.
	host, err := p.Resolve(storageID, sandboxPath)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fi, err := os.Stat(host)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", sandboxPath)
	}

	dir, err := os.Open(host)
	if err != nil {
		return nil, err
	}
	defer dir.Close()

	entries, err := dir.ReadDir(limit + 1)
	if err != nil {
		return nil, err
	}
	res := &Listing{Storage: storageID, Path: sandboxPath}
	if len(entries) > limit {
		res.Truncated = true
		entries = entries[:limit]
	}
	for _, e := range entries {
		isDir := e.IsDir()
		// A symlink is reported as its target's kind so the UI does not offer
		// a "directory" that the jail will later refuse.
		if e.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(filepath.Join(host, e.Name())); err == nil {
				isDir = fi.IsDir()
			} else {
				continue // dangling or escaping symlink: not listable
			}
		}
		if sel == SelectFile && isDir {
			continue
		}
		if sel == SelectDirectory && !isDir {
			continue
		}
		entry := Entry{
			Name:  e.Name(),
			Path:  joinSandbox(sandboxPath, e.Name()),
			IsDir: isDir,
		}
		if info, err := e.Info(); err == nil {
			entry.Size = info.Size()
			entry.ModTime = info.ModTime()
		}
		res.Entries = append(res.Entries, entry)
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		if res.Entries[i].IsDir != res.Entries[j].IsDir {
			return res.Entries[i].IsDir // directories first
		}
		return strings.ToLower(res.Entries[i].Name) < strings.ToLower(res.Entries[j].Name)
	})
	return res, nil
}

func joinSandbox(base, name string) string {
	if base == "/" {
		return "/" + name
	}
	return base + "/" + name
}

// CheckReachable verifies that the OS user running SRCOS can actually read a
// storage root.
//
// It is a separate call from New because it touches the filesystem, and New is
// also used in tests and by the CLI's configuration validation. Bind mounts do
// not change permissions — a root SRCOS cannot read is a root the sandbox
// cannot read either — so this is worth reporting loudly at startup.
func (p *fileProvider) CheckReachable() []string {
	var problems []string
	for _, s := range p.storages {
		if _, err := os.Stat(s.HostRoot); err != nil {
			problems = append(problems, fmt.Sprintf("storage %q: host_root %s is not accessible: %v "+
				"(bind mounts do not change permissions, so the sandbox could not read it either — "+
				"grant SRCOS's OS user access with e.g. `setfacl -R -m u:<osuser>:r-x %s`)",
				s.ID, s.HostRoot, err, s.HostRoot))
			continue
		}
		f, err := os.Open(s.HostRoot)
		if err != nil {
			problems = append(problems, fmt.Sprintf("storage %q: host_root %s is not readable: %v", s.ID, s.HostRoot, err))
			continue
		}
		f.Close()
	}
	return problems
}

// ReachabilityChecker is implemented by providers that can validate their
// host roots against the current process's permissions.
type ReachabilityChecker interface{ CheckReachable() []string }

// MountsFor renders the storages a tool requires into MountSpec entries.
//
// The runtime calls this; the provider owns the mapping because it alone knows
// each storage's host root and sandbox path.
func MountsFor(spec *sandbox.Spec, storages []Storage) error {
	for _, s := range storages {
		mode := sandbox.ReadOnly
		if s.Mode == ReadWrite {
			mode = sandbox.ReadWrite
		}
		if err := spec.Add(sandbox.Mount{
			HostPath:    s.HostRoot,
			SandboxPath: s.SandboxPath,
			Mode:        mode,
			Origin:      OriginFor(s.ID),
		}); err != nil {
			return err
		}
	}
	return nil
}
