package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// ValidatePathParams enforces the storage binding of file/directory/path
// inputs at the write path.
//
// The interface declares `from: <storage>`; the path picker only ever offers
// paths inside those storages, but a REST/MCP caller is not the picker. Without
// this check, an agent can submit any string for a storage-bound input — inside
// a bwrap sandbox it usually fails to exist, but with `sandbox: none` it is a
// real host path, and even under bwrap the declared relation between the input
// and the tool's storage would be a convention rather than a contract.
//
// Only inputs with a `from` are checked here. A raw `file`/`directory` input
// (no `from`) is a sandbox path chosen by the caller; the sandbox's mount table,
// not this function, is its boundary.
func ValidatePathParams(t *tool.Tool, j *job.Job, storages storage.Provider) error {
	if t == nil || j == nil {
		return nil
	}
	params := job.EffectiveParams(j, t)
	for _, in := range t.Interface.Inputs {
		switch in.Type {
		case tool.TypeFile, tool.TypeDirectory, tool.TypeDirPath, tool.TypePath:
		default:
			continue
		}
		raw, present := params[in.Name]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			// job.Validate has already rejected a non-string; keep this
			// function independent of that ordering.
			return fmt.Errorf("input %s: want a path string, got %T", in.Name, raw)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}

		ids := splitStorageIDs(in.From)
		if len(ids) == 0 {
			continue
		}
		if storages == nil {
			return fmt.Errorf("input %s: storage-bound but no StorageProvider is configured", in.Name)
		}

		var host string
		var lastErr error
		for _, id := range ids {
			h, err := storages.Resolve(id, value)
			if err == nil {
				host = h
				break
			}
			lastErr = err
		}
		if host == "" {
			return fmt.Errorf("input %s: %q is not inside the declared storage(s) %v: %v",
				in.Name, value, ids, lastErr)
		}

		// A `path` without `select` may name a directory or file that the tool
		// will create (an output), so only compare the kind when the interface
		// actually declares one. `file`/`directory` are inputs and must exist.
		want := ""
		switch in.Type {
		case tool.TypeFile:
			want = "file"
		case tool.TypeDirectory, tool.TypeDirPath:
			want = "directory"
		case tool.TypePath:
			want = in.Select
		}
		fi, err := os.Stat(host)
		if err != nil {
			if in.Type == tool.TypePath && want == "" {
				continue // containment checked; existence is the tool's business
			}
			return fmt.Errorf("input %s: %q: %v", in.Name, value, err)
		}
		switch want {
		case "file":
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("input %s: %q is not a regular file", in.Name, value)
			}
		case "directory":
			if !fi.IsDir() {
				return fmt.Errorf("input %s: %q is not a directory", in.Name, value)
			}
		}
		if fi.IsDir() {
			for _, name := range in.Files {
				if _, err := os.Stat(filepath.Join(host, name)); err != nil {
					return fmt.Errorf("input %s: directory %q does not contain %s", in.Name, value, name)
				}
			}
		}
	}
	return nil
}

// splitStorageIDs is the runtime copy of the contract's comma-separated list.
// It lives here rather than importing an unexported helper from package tool.
func splitStorageIDs(from string) []string {
	var out []string
	for _, p := range strings.Split(from, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
