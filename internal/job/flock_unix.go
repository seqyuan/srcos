//go:build !windows

package job

import (
	"errors"
	"os"
	"syscall"
)

// errLeaseHeld is "someone else holds this lease", not a failure.
var errLeaseHeld = errors.New("lease held by another runner")

// lockFile takes a non-blocking exclusive flock. The lock lives on the open
// file description, so closing the file (including on process death) releases
// it; that is what makes a claim self-healing after a crash.
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errLeaseHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
