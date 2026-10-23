//go:build linux

package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// VerifyOpen checks that f's descriptor still names a path inside root.
//
// It is the post-open half of the jail: resolving a path and opening it are two
// syscalls, and a tool with write access to the mount can swap the path in
// between. /proc/self/fd reports the file the descriptor actually refers to, so
// the check is about the opened object, not the path that was looked up.
func VerifyOpen(f *os.File, root string) error {
	if f == nil {
		return fmt.Errorf("%w: nil file", ErrEscapes)
	}
	root = filepath.Clean(root)
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}

	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil {
		return fmt.Errorf("%w: cannot read the opened descriptor: %v", ErrEscapes, err)
	}
	// A deleted file still reports its last path with this suffix. The path is
	// under the mount or it is not; the suffix does not change that.
	target = strings.TrimSuffix(target, " (deleted)")
	if !filepath.IsAbs(target) {
		return fmt.Errorf("%w: descriptor names an anonymous file %q", ErrEscapes, target)
	}
	if real, err := filepath.EvalSymlinks(target); err == nil {
		target = real
	}

	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s is outside %s", ErrEscapes, target, root)
	}
	return nil
}
