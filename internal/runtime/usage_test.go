package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The admin page shows usage next to the declared ceiling, so the snapshot has
// to be about *this* process: a reused pid's counters belong to someone else.
func TestLocalUnitUsageReadsTheRecordedProcess(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("/proc is needed for usage sampling")
	}
	loc := &Local{SystemdUser: boolPtr(false)}
	ctx := context.Background()

	// The fixture reports readiness *after* allocating, so the test measures a
	// real reading rather than the interpreter's startup.
	marker := filepath.Join(t.TempDir(), "ready")
	code := fmt.Sprintf("import time\nx = bytearray(20*1024*1024)\nfor b in range(0, len(x), 4096): x[b] = 1\nopen(%q, 'w').close()\ntime.sleep(30)\n", marker)
	pid, start := startChild(t, "python3", "-c", code)
	waitForFile(t, marker)

	usage, ok := loc.UnitUsage(ctx, &Instance{PID: pid, PIDStart: start})
	if !ok {
		t.Fatal("a live recorded process must be samplable")
	}
	if usage.RSSBytes < 10*1024*1024 {
		t.Fatalf("RSS = %d, want at least the 20 MiB the fixture allocated", usage.RSSBytes)
	}
	if usage.SampledAt.IsZero() {
		t.Fatal("a sample must be timestamped")
	}

	// A stale identity is not this instance: report nothing rather than
	// somebody else's numbers.
	if _, ok := loc.UnitUsage(ctx, &Instance{PID: pid, PIDStart: start + 1}); ok {
		t.Fatal("a reused pid must not be sampled")
	}
	if _, ok := loc.UnitUsage(ctx, &Instance{PID: pid}); ok {
		t.Fatal("an unverifiable record must not be sampled")
	}
	if _, ok := loc.UnitUsage(ctx, &Instance{}); ok {
		t.Fatal("a record with no pid must not be sampled")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := map[uint64]string{
		0:               "0 B",
		512:             "512 B",
		2048:            "2.0 KiB",
		5 * 1024 * 1024: "5.0 MiB",
		3 * 1 << 30:     "3.0 GiB",
		2 * (1 << 40):   "2.0 TiB",
	}
	for in, want := range cases {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestedResources(t *testing.T) {
	inst := &Instance{RequestedCPU: 4, RequestedMemory: "8Gi"}
	cpu, mem := inst.RequestedResources()
	if cpu != 4 || mem != "8Gi" {
		t.Fatalf("got %d %q", cpu, mem)
	}
	var nilInst *Instance
	if cpu, mem := nilInst.RequestedResources(); cpu != 0 || mem != "" {
		t.Fatalf("a nil instance has no ceiling: %d %q", cpu, mem)
	}
}
