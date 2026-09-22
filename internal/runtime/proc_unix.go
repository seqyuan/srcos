package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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

// pidStartTime reads a process's start time from /proc/<pid>/stat (field 22,
// in clock ticks since boot).
//
// Together with the pid it identifies one *incarnation* of a process, which is
// what makes it safe to act on a pid read back from a file: the number alone is
// reused by the OS, so a stale record could otherwise signal an unrelated
// process. The second return value is false when the process is gone or /proc
// cannot be read (a non-Linux host), and callers must treat "unknown" as "do
// not signal".
func pidStartTime(pid int) (uint64, bool) {
	if pid <= 0 {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, false
	}
	// Field 2 is the command name in parentheses and may itself contain spaces
	// and parentheses, so parse from the last ')'. After it come state, ppid, …
	// with starttime at field 22 = index 19 of what remains.
	i := bytes.LastIndexByte(data, ')')
	if i < 0 || i+2 > len(data) {
		return 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}

// stopGracePeriod is how long a service gets to exit on SIGTERM before it is
// killed. A variable because it is the one part of this path worth testing
// without waiting ten seconds for it.
var stopGracePeriod = 10 * time.Second

// stopRecordedProcess ends a process that SRCOS started, after verifying that
// the pid still refers to that same process.
//
// The verification is the whole reason this is safe: a pid read back from a
// record may have been reused by an unrelated process in the meantime. The
// mismatch cases return nil rather than an error — "our process is gone" is a
// successful stop, and the caller records the instance as stopped.
func stopRecordedProcess(ctx context.Context, pid int, start uint64) error {
	if pid <= 0 {
		return nil
	}

	current, ok := pidStartTime(pid)
	if !ok {
		// Gone already, or /proc is unavailable on this host.
		if !processAlive(pid) {
			return nil
		}
		return fmt.Errorf("process %d is running but its identity cannot be verified (no /proc entry), refusing to signal it", pid)
	}
	if start == 0 {
		return fmt.Errorf("process %d has no recorded start time, refusing to signal a pid that could have been reused", pid)
	}
	if current != start {
		return nil // the pid was reused: our process exited on its own
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop process %d: %w", pid, err)
	}

	// Give it a chance to shut down on its own terms before insisting. A
	// service that ignores SIGTERM (a badly behaved tool) is worth waiting a
	// few seconds for rather than corrupting whatever it was writing.
	deadline := time.Now().Add(stopGracePeriod)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}

	// SIGKILL cannot be ignored, but a process we are the parent of stays
	// visible as a zombie until it is reaped. Waiting here makes "stopped" mean
	// stopped — the next `svc stop` should not find a ghost, and a caller that
	// immediately restarts the service should not trip over the old pid.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("process %d is still present after SIGKILL", pid)
}
