package safefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestMkdirAllRefusesSymlinkComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := MkdirAllBeneath(root, filepath.Join(root, "link", "sub"), 0o755); err == nil {
		t.Fatal("MkdirAll followed a symlink component")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); err == nil {
		t.Fatal("the symlink target was modified")
	}
}

func TestWriteFileRefusesSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "file")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteFileBeneath(root, link, []byte("pwned"), 0o600); err == nil {
		t.Fatal("WriteFile followed a symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Fatalf("target content changed to %q", data)
	}
}

func TestVerifyOpenAcceptsFileInsideRootAndRejectsOutside(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "a.txt")
	if err := os.WriteFile(inside, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(inside)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyOpen(f, root); err != nil {
		t.Fatalf("inside file rejected: %v", err)
	}
	f.Close()

	outside := filepath.Join(t.TempDir(), "b.txt")
	if err := os.WriteFile(outside, []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(outside)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := VerifyOpen(f, root); err == nil {
		t.Fatal("outside file accepted")
	}
}

func TestVerifyOpenDetectsSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "file")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Open follows the symlink; VerifyOpen must notice that the descriptor
	// names a path outside root.
	f, err := os.Open(link)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	verifyErr := VerifyOpen(f, root)
	if verifyErr == nil {
		t.Fatal("a descriptor outside the mount was accepted")
	}
	if !strings.Contains(verifyErr.Error(), "outside") {
		t.Fatalf("unexpected error: %v", verifyErr)
	}
}

// The trusted root is administrator state and may be reached through a symlink
// (a deployment can mount /data elsewhere). Only components *below* it must be
// real directories.
func TestMkdirAllBeneathAllowsASymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	root := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	target := filepath.Join(root, "ws", "alice")
	if err := MkdirAllBeneath(root, target, 0o755); err != nil {
		t.Fatalf("MkdirAllBeneath through a symlinked root: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(real, "ws", "alice")); err != nil || !fi.IsDir() {
		t.Fatalf("target was not created under the real root: %v", err)
	}
}

func TestWriteFileAtomicReplacesWithoutLeftoverTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.yaml")
	if err := WriteFileAtomic(path, []byte("one"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("two"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "two" {
		t.Fatalf("content = %q", data)
	}
	temps, _ := filepath.Glob(filepath.Join(dir, ".state.yaml.tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("leftover temp files: %v", temps)
	}
}

func TestWriteFileAtomicExclusiveRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "job.json")
	if err := WriteFileAtomicExclusive(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomicExclusive(path, []byte("second"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second write = %v, want os.ErrExist", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "first" {
		t.Fatalf("the existing file was overwritten: %q", data)
	}
}

func TestWriteFileAtomicExclusiveFallsBackWithoutHardLinks(t *testing.T) {
	old := linkFile
	linkFile = func(_, _ string) error {
		return &os.LinkError{Op: "link", Old: "a", New: "b", Err: syscall.EOPNOTSUPP}
	}
	defer func() { linkFile = old }()

	dir := t.TempDir()
	path := filepath.Join(dir, "job.json")
	if err := WriteFileAtomicExclusive(path, []byte("fallback"), 0o644); err != nil {
		t.Fatalf("fallback write: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "fallback" {
		t.Fatalf("content = %q", data)
	}
	if err := WriteFileAtomicExclusive(path, []byte("again"), 0o644); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback repeat = %v, want os.ErrExist", err)
	}
	temps, _ := filepath.Glob(filepath.Join(dir, ".job.json.tmp-*"))
	if len(temps) != 0 {
		t.Fatalf("leftover temp files: %v", temps)
	}
}
