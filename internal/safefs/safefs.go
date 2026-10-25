// Package safefs provides the small set of filesystem operations SRCOS needs
// for paths inside a tool-writable tree: the workspace, the virtual home and
// the flow-run directory are mounted read-write into the sandbox, so a tool can
// plant a symlink there and wait for SRCOS to follow it.
//
// Two classes of operation are covered:
//
//   - Writes (MkdirAllBeneath/WriteFileBeneath/CreateBeneath) refuse to traverse
//     or replace a symlink below a trusted root. The root itself may legitimately
//     be reached through a symlink (a deployment can mount /data elsewhere); it
//     is resolved once and everything below it is checked component by component.
//   - VerifyOpen checks, after a file has been opened, that the descriptor
//     really points inside the expected mount. It closes the check-then-open
//     window left by sandbox.Spec.ResolveExisting: an attacker who swaps a
//     regular file for a symlink between the check and the open is caught
//     before any byte is returned.
//
// This is not a replacement for openat2(RESOLVE_BENEATH): a concurrent swap of
// an *intermediate* directory after the check is still theoretically possible.
// The helpers remove the static and the practical races; the remaining window
// can be closed by moving to openat2 later.
package safefs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MkdirAllBeneath creates dir (and its missing parents) below root without
// following a symlink in any component below root.
//
// root is trusted: it is the administrator-created data directory, and may
// itself be a symlink. It is resolved once; the components walked below it are
// required to be real directories.
func MkdirAllBeneath(root, dir string, perm os.FileMode) error {
	if root == "" {
		return fmt.Errorf("safefs: empty trusted root")
	}
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)

	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("safefs: %s is not beneath %s", dir, root)
	}

	// The root is trusted; create it if missing and resolve any symlink in its
	// own path. Nothing below it gets the same benefit.
	// The root is administrator state, not the tool's directory: keep its
	// conventional 0755 even when the child needs 0700.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("safefs: resolve root %s: %w", root, err)
	}
	if rel == "." {
		return nil
	}

	cur := realRoot
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		if comp == "" || comp == "." {
			continue
		}
		if comp == ".." {
			return fmt.Errorf("safefs: %s escapes %s", dir, root)
		}
		cur = filepath.Join(cur, comp)

		fi, err := os.Lstat(cur)
		switch {
		case err == nil:
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s is a symlink (refusing to write through it)", cur)
			}
			if !fi.IsDir() {
				return fmt.Errorf("%s is not a directory", cur)
			}
			continue
		case !os.IsNotExist(err):
			return err
		}

		if err := os.Mkdir(cur, perm); err != nil {
			// Another process may have created it between Lstat and Mkdir.
			// Accept only a real directory; a symlink that won the race is
			// still refused.
			if fi, statErr := os.Lstat(cur); statErr == nil {
				if fi.Mode()&os.ModeSymlink != 0 {
					return fmt.Errorf("%s is a symlink (refusing to write through it)", cur)
				}
				if fi.IsDir() {
					continue
				}
			}
			return err
		}
	}
	return nil
}

// WriteFileBeneath writes data to path below root, creating parents without
// following symlinks and refusing to overwrite through a symlink.
func WriteFileBeneath(root, path string, data []byte, perm os.FileMode) error {
	if err := MkdirAllBeneath(root, filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// CreateBeneath is os.Create below root, without following symlinks.
func CreateBeneath(root, path string, perm os.FileMode) (*os.File, error) {
	if err := MkdirAllBeneath(root, filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
}

// OpenFile is os.OpenFile with O_NOFOLLOW, so the final path component cannot
// be a symlink. Parent directories are the caller's responsibility (use
// MkdirAllBeneath), which keeps the "check the parents" decision explicit.
func OpenFile(path string, flags int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flags|syscall.O_NOFOLLOW, perm)
}

// ErrEscapes is returned when an opened descriptor points outside the mount it
// was resolved from.
var ErrEscapes = errors.New("opened path escapes its mount")

// linkFile is os.Link, behind a variable so a test can exercise the
// filesystem-does-not-support-hardlinks fallback.
var linkFile = os.Link

// WriteFileAtomic publishes a file by writing a unique sibling temp file and
// renaming it into place. A reader sees either the old file or the new one,
// never a half-written body.
//
// It does not fsync: callers that need crash durability use
// WriteFileAtomicSynced. The directory must already be trusted/validated
// (MkdirAllBeneath); the rename replaces a symlink at the final component
// rather than following it, which is the intended difference from
// WriteFileBeneath's O_NOFOLLOW refusal.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, false)
}

// WriteFileAtomicSynced is WriteFileAtomic plus a file and directory fsync, for
// state that must survive a crash (a submission's publication marker, the
// authorization policy).
func WriteFileAtomicSynced(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(path, data, perm, true)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode, synced bool) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if synced {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if synced {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}

// WriteFileAtomicExclusive publishes a file only if the target does not already
// exist. It returns os.ErrExist (errors.Is) when it lost that race.
//
// It writes a unique temp file first, then hard-links it into place. Hard links
// are not universal (CIFS without unix extensions, some FUSE mounts, FAT), so an
// unsupported link falls back to O_CREATE|O_EXCL on the final path: exclusive
// but not atomic. The drop-box tolerates that: job.Scan reports a transient
// unparsable job.json as Broken and the next tick retries.
func WriteFileAtomicExclusive(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := linkFile(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		if linkUnsupported(err) {
			return writeExclusiveDirect(path, data, perm)
		}
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func writeExclusiveDirect(path string, data []byte, perm os.FileMode) error {
	f, err := OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// linkUnsupported reports whether err means "this filesystem has no hard
// links", as opposed to a real failure.
func linkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.ENOTSUP)
}
