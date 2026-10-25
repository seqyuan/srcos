//go:build windows

package job

import (
	"errors"
	"os"
)

var errLeaseHeld = errors.New("lease held by another runner")

// SRCOS runs on Linux; this stub keeps the package buildable elsewhere. It is
// process-local only, so a Windows deployment would not get cross-process
// exclusion — which is honest, since the runtime does not support Windows.
func lockFile(_ *os.File) error   { return nil }
func unlockFile(_ *os.File) error { return nil }
