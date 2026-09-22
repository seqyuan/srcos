package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitFromTemplate copies a template directory into target exactly once.
//
// The marker lives inside the target: a user who deliberately deletes the
// seeded files should not have them silently restored on the next run.
func InitFromTemplate(templateDir, target string) error {
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
	if err := CopyTree(templateDir, target); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte("initialized from "+templateDir+"\n"), 0o644)
}

// CopyTree copies regular files and directories, preserving permission bits.
func CopyTree(src, dst string) error {
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
			return os.MkdirAll(out, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil // skip sockets/devices; templates never need them
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, fi.Mode().Perm())
	})
}
