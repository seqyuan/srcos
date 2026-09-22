package runtime

import "syscall"

// processAlive reports whether a pid exists and we may signal it.
//
// Signal 0 performs the permission and existence checks without delivering
// anything, which is the portable way to ask "is this process still there".
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
