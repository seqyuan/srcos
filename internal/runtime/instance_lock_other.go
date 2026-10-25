//go:build windows

package runtime

import "os"

// SRCOS runs on Linux; this stub keeps the package buildable elsewhere.
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
	return &instanceLock{file: f}, nil
}

func (l *instanceLock) release() {
	if l != nil && l.file != nil {
		_ = l.file.Close()
	}
}
