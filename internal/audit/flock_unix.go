//go:build !windows

package audit

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, so one appender at a time
// can read the chain head and write the next link.
//
// The lock is advisory and per-open-file-description (flock semantics), which
// is what a same-machine CLI + gateway pair needs; the audit stream lives on
// the machine that writes it.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
