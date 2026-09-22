package inspect

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// DefaultReadBytes / MaxReadBytes cap a single text read.
const (
	DefaultReadBytes = 64 * 1024
	MaxReadBytes     = 256 * 1024
)

// ReadRequest is one file-read request.
type ReadRequest struct {
	// Path is a sandbox path: /home/<user>/..., /workspace/... or a storage
	// root. Host paths are never accepted (and never returned).
	Path string
	// Tool is required for a /workspace path, because the workspace is a mount
	// point whose host directory depends on the tool (ADR-021).
	Tool string
	// MaxBytes caps the read; the answer says whether it was truncated.
	MaxBytes int64
}

// FileContent is one text file's content, with the sandbox path it was read
// from.
type FileContent struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated,omitempty"`
	Text      string `json:"text"`
}

// ReadFile reads a text file from the range this user may read.
//
// The range is deliberately narrow, and each part of it answers a different
// question a reader actually has:
//
//   - `/home/<user>/...`     — the virtual home: caches, notebooks, dotfiles
//   - `/workspace/...`       — a tool's workspace (the `tool` argument names which)
//   - a storage's sandbox path — shared reference data and project directories
//
// Everything else is refused, including `/tool` (the tool package: an agent
// reads a tool's *interface*, not its implementation) and any host path. The
// resolution itself goes through the same sandbox.Spec the runtime mounts, so
// `..`, absolute host paths and symlink escapes are refused by the one
// implementation that also guards the sandbox.
func (r *Reader) ReadFile(username string, req ReadRequest) (*FileContent, error) {
	path := strings.TrimSpace(req.Path)
	if path == "" {
		return nil, fmt.Errorf("%w: path is required", ErrBadRequest)
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("%w: path must be a sandbox path starting with / (host paths are not readable)", ErrBadRequest)
	}

	spec, err := r.readSpec(username, req.Tool)
	if err != nil {
		return nil, err
	}
	host, err := spec.ResolveExisting(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}

	max := req.MaxBytes
	if max <= 0 {
		max = DefaultReadBytes
	}
	if max > MaxReadBytes {
		max = MaxReadBytes
	}

	fi, err := os.Stat(host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("%w: %s is a directory (use srcos_list_paths)", ErrBadRequest, path)
	}

	f, err := os.Open(host)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	truncated := int64(len(data)) > max
	if truncated {
		data = data[:max]
	}
	// Text only: a caller that asked for bytes is a viewer's job, and handing
	// back binary as a string helps nobody. The byte cap can land in the middle
	// of a multi-byte rune, so drop the partial tail before judging the content.
	data = validPrefix(data)
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: %s is not a text file", ErrBadRequest, path)
	}

	return &FileContent{
		Path:      path,
		Size:      fi.Size(),
		Truncated: truncated,
		Text:      string(data),
	}, nil
}

// readSpec builds the mount table a user-level read is allowed to see: the
// virtual home, plus (when a tool is named) that tool's workspace, plus the
// storages the user's visible tools declare.
func (r *Reader) readSpec(username, toolID string) (*sandbox.Spec, error) {
	spec := &sandbox.Spec{}
	home := config.HomeDir(r.ConfigDir, username)
	if err := spec.Add(sandbox.Mount{
		HostPath:    home,
		SandboxPath: sandbox.HomePath(username),
		Mode:        sandbox.ReadWrite,
		Origin:      "builtin",
	}); err != nil {
		return nil, err
	}

	if strings.TrimSpace(toolID) != "" {
		t, err := r.Manifest(toolID)
		if err != nil {
			return nil, err
		}
		if err := spec.Add(sandbox.Mount{
			HostPath:    config.WorkspaceDir(r.ConfigDir, username, t.ID),
			SandboxPath: sandbox.PathWorkspace,
			Mode:        sandbox.ReadWrite,
			Origin:      "builtin",
		}); err != nil {
			return nil, err
		}
		// Only this tool's storages: naming a tool narrows the range, so a
		// caller cannot use the tool argument to reach another tool's data.
		sts, err := r.toolStorages(t)
		if err != nil {
			return nil, err
		}
		if err := storage.MountsFor(spec, sts); err != nil {
			return nil, err
		}
		return spec, nil
	}

	// No tool named: the union of what this user's tools declare. It is the
	// same set srcos_list_storages reports, so the listing and the read range
	// stay one rule (ADR-020).
	sts, err := r.visibleStorages(username)
	if err != nil {
		return nil, err
	}
	if err := storage.MountsFor(spec, sts); err != nil {
		return nil, err
	}
	return spec, nil
}

// toolStorages resolves a tool's declared storages (registration already
// checked the ids, so a failure here means the declaration changed under us).
func (r *Reader) toolStorages(t *tool.Tool) ([]storage.Storage, error) {
	if len(t.RequiresStorages) == 0 || r.Storages == nil {
		return nil, nil
	}
	sts, err := r.Storages.ForTool(t.RequiresStorages)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return sts, nil
}

// visibleStorages is the union of the storages the user's visible tools
// declare.
func (r *Reader) visibleStorages(username string) ([]storage.Storage, error) {
	if r.Storages == nil {
		return nil, nil
	}
	manifests, err := r.VisibleManifests(username)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []storage.Storage
	for _, t := range manifests {
		for _, id := range t.RequiresStorages {
			if seen[id] {
				continue
			}
			s, ok := r.Storages.Get(id)
			if !ok {
				continue
			}
			seen[id] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// validPrefix drops an incomplete trailing UTF-8 sequence, which a byte cap
// can leave behind.
//
// It drops only a *truncated* multi-byte sequence: a genuinely invalid byte
// (0xFF, a stray continuation byte) is left where it is, so the text check
// downstream can reject the file instead of this function silently masking it.
func validPrefix(data []byte) []byte {
	if len(data) == 0 || utf8.Valid(data) {
		return data
	}
	start := len(data) - 1
	for start > 0 && !utf8.RuneStart(data[start]) && len(data)-start < utf8.UTFMax {
		start--
	}
	if !utf8.RuneStart(data[start]) {
		return data
	}
	want := runeBytes(data[start])
	if want < 0 {
		return data // not a rune start at all: the invalidity is real
	}
	if len(data)-start < want {
		return data[:start] // the cap cut the rune in half
	}
	return data
}

// runeBytes returns how many bytes a UTF-8 sequence starting with b occupies,
// or -1 when b cannot start one.
func runeBytes(b byte) int {
	switch {
	case b&0x80 == 0:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	}
	return -1
}
