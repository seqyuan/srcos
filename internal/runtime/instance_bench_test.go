package runtime

import (
	"fmt"
	"testing"
	"time"
)

// benchInstance builds a record of the shape the scan tick reads: every tick
// runs ListInstances six times, so this measures the unit cost that the
// snapshot-sharing refactor (#2-a) is meant to divide by six.
func benchInstance(i int) *Instance {
	now := time.Now().UTC()
	return &Instance{
		ID:        fmt.Sprintf("alice-bench-%06d", i),
		User:      "alice",
		Tool:      "bench",
		Kind:      "task",
		State:     StateSucceeded,
		StartedAt: now,
		EndedAt:   now,
	}
}

// seedInstances writes n records once (untimed) and returns the config dir.
func seedInstances(b *testing.B, n int) string {
	b.Helper()
	dir := b.TempDir()
	for i := 0; i < n; i++ {
		path := InstancePath(dir, fmt.Sprintf("alice-bench-%06d", i))
		if err := SaveInstance(path, benchInstance(i)); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// BenchmarkListInstances is the #2 baseline: one full readdir + read + parse of
// every record. A tick pays it six times (plus extra syncRoutes on a hit).
func BenchmarkListInstances(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			dir := seedInstances(b, n)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ListInstances(dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
