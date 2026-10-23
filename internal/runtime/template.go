package runtime

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/seqyuan/srcos/internal/safefs"
)

// InitFromTemplate copies a template directory into target exactly once.
//
// The marker lives inside the target: a user who deliberately deletes the
// seeded files should not have them silently restored on the next run.
//
// This wrapper has no trusted root and is kept for callers that only have a
// throwaway directory (tests). Production uses initTemplate with the data
// directory as the root.
func InitFromTemplate(templateDir, target string) error {
	return initTemplate("", templateDir, target)
}

// initTemplate is InitFromTemplate with a symlink-safe root. When root is
// non-empty, every directory and file below it is created/opened without
// following a symlink; when it is empty (tests) the plain os operations are
// used.
func initTemplate(root, templateDir, target string) error {
	info, err := os.Stat(templateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("init_from %q does not exist", templateDir)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("init_from %q is not a directory", templateDir)
	}
	marker := filepath.Join(target, ".srcos-initialized")
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	if err := copyTree(root, templateDir, target); err != nil {
		return err
	}
	return writeFile(root, marker, []byte("initialized from "+templateDir+"\n"), 0o644)
}

// CopyTree copies regular files and directories, preserving permission bits.
func CopyTree(src, dst string) error { return copyTree("", src, dst) }

func copyTree(root, src, dst string) error {
	return filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return mkdir(root, out)
		}
		if !fi.Mode().IsRegular() {
			return nil // skip sockets/devices; templates never need them
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeFile(root, out, data, fi.Mode().Perm())
	})
}

func mkdir(root, dir string) error {
	if root == "" {
		return os.MkdirAll(dir, 0o755)
	}
	return safefs.MkdirAllBeneath(root, dir, 0o755)
}

func writeFile(root, path string, data []byte, perm os.FileMode) error {
	if root == "" {
		return os.WriteFile(path, data, perm)
	}
	return safefs.WriteFileBeneath(root, path, data, perm)
}
