package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// waitForFile blocks until a fixture signals that it is in the state the test
// needs (handlers installed, listener bound), or fails after a few seconds.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// startChild starts a real process and returns its pid and start time, with a
// cleanup that guarantees it is gone even if the test fails early.
//
// The wait goroutine matters: a killed child that nobody reaps stays a zombie,
// and a zombie still answers signal 0 — which is exactly the confusion these
// tests exist to rule out.
func startChild(t *testing.T, argv ...string) (int, uint64) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("child %d did not exit", cmd.Process.Pid)
		}
	})

	start, ok := pidStartTime(cmd.Process.Pid)
	if !ok {
		t.Skipf("/proc is unavailable, cannot exercise pid identity")
	}
	return cmd.Process.Pid, start
}

func TestPidStartTimeIsStableForOneProcess(t *testing.T) {
	pid, start := startChild(t, "sleep", "60")
	again, ok := pidStartTime(pid)
	if !ok || again != start {
		t.Fatalf("start time changed between reads: %d then %d (ok=%v)", start, again, ok)
	}
	// Our own pid works too, and the zero pid never does.
	if _, ok := pidStartTime(0); ok {
		t.Fatal("pid 0 has no /proc entry")
	}
}

func TestStopRecordedProcessEndsIt(t *testing.T) {
	pid, start := startChild(t, "sleep", "60")

	if err := stopRecordedProcess(context.Background(), pid, start); err != nil {
		t.Fatalf("stopRecordedProcess: %v", err)
	}
	if processAlive(pid) {
		t.Fatal("the process survived the stop")
	}
	// Stopping again is not an error: the record is simply already satisfied.
	if err := stopRecordedProcess(context.Background(), pid, start); err != nil {
		t.Fatalf("a second stop must succeed: %v", err)
	}
}

// A pid read back from a record may have been reused. Killing on the number
// alone would kill an unrelated process, so the start time has to match.
func TestStopRecordedProcessRefusesAReusedPid(t *testing.T) {
	pid, start := startChild(t, "sleep", "60")

	if err := stopRecordedProcess(context.Background(), pid, start+1); err != nil {
		t.Fatalf("a stale record must be treated as already gone, got %v", err)
	}
	if !processAlive(pid) {
		t.Fatal("a process whose recorded identity does not match must not be signalled")
	}
}

// Without a recorded start time there is nothing to verify against, so the
// stop refuses instead of gambling on the pid.
func TestStopRecordedProcessRefusesAnUnverifiablePid(t *testing.T) {
	pid, _ := startChild(t, "sleep", "60")

	err := stopRecordedProcess(context.Background(), pid, 0)
	if err == nil {
		t.Fatal("an unverifiable pid must be refused")
	}
	if !processAlive(pid) {
		t.Fatal("the process must be left alone when it cannot be verified")
	}
}

func TestStopRecordedProcessOnAGoneProcessIsSuccess(t *testing.T) {
	pid, start := startChild(t, "sleep", "60")
	if err := stopRecordedProcess(context.Background(), pid, start); err != nil {
		t.Fatal(err)
	}
	if err := stopRecordedProcess(context.Background(), pid, start); err != nil {
		t.Fatalf("an already-gone process is a successful stop: %v", err)
	}
	if err := stopRecordedProcess(context.Background(), 0, 0); err != nil {
		t.Fatalf("no pid is nothing to stop: %v", err)
	}
}

// A tool that ignores SIGTERM still has to be stopped.
func TestStopRecordedProcessEscalatesToSIGKILL(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for a process that reliably ignores SIGTERM")
	}
	old := stopGracePeriod
	stopGracePeriod = 300 * time.Millisecond
	t.Cleanup(func() { stopGracePeriod = old })

	// A shell fixture does not work here: dash replaces itself with the last
	// command, so the trap is gone. The marker file is not decoration either —
	// a SIGTERM delivered while the interpreter is still starting kills it
	// before the handler exists, which would make this test measure startup
	// time instead of the escalation.
	marker := filepath.Join(t.TempDir(), "ready")
	code := fmt.Sprintf("import signal, time; signal.signal(signal.SIGTERM, signal.SIG_IGN); open(%q, 'w').close(); time.sleep(60)", marker)
	pid, start := startChild(t, "python3", "-c", code)
	waitForFile(t, marker)

	started := time.Now()
	if err := stopRecordedProcess(context.Background(), pid, start); err != nil {
		t.Fatalf("stopRecordedProcess: %v", err)
	}
	if processAlive(pid) {
		t.Fatal("a process ignoring SIGTERM must be killed")
	}
	if elapsed := time.Since(started); elapsed < stopGracePeriod {
		t.Fatalf("the grace period was skipped (%s)", elapsed)
	}
}

// The degraded mode has no unit to ask, so the recorded pid identity is the
// only liveness signal — and a stale one must not read as alive.
func TestUnitAliveUsesTheRecordedPidIdentity(t *testing.T) {
	loc := &Local{SystemdUser: boolPtr(false)}
	ctx := context.Background()
	pid, start := startChild(t, "sleep", "60")

	if !loc.UnitAlive(ctx, &Instance{PID: pid, PIDStart: start}) {
		t.Fatal("a live recorded process must count as alive")
	}
	if loc.UnitAlive(ctx, &Instance{PID: pid, PIDStart: start + 1}) {
		t.Fatal("a reused pid must not count as alive")
	}
	if loc.UnitAlive(ctx, &Instance{PID: pid}) {
		t.Fatal("an unverifiable record must not count as alive")
	}
	if loc.UnitAlive(ctx, &Instance{}) {
		t.Fatal("a record with no pid must not count as alive")
	}
	if loc.UnitAlive(ctx, &Instance{PID: 1 << 30, PIDStart: 1}) {
		t.Fatal("a pid that does not exist must not count as alive")
	}
}
