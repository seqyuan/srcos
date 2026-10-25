//go:build !windows

package runtime

import (
	"os"
	"path/filepath"
	"syscall"
)

// instanceLock serializes read-modify-write updates of one instance record.
// The flock is blocking: a second writer waits for the first, then reloads the
// record it just wrote.
type instanceLock struct {
	file *os.File
}

func lockInstance(path string) (*instanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &instanceLock{file: f}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}
