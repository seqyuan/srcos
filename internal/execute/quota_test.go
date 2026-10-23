package execute

import (
	"testing"

	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

type fixedGrants struct {
	allowed bool
	quota   grant.Quota
}

func (f fixedGrants) Allowed(string, string) bool         { return f.allowed }
func (f fixedGrants) QuotaFor(string, string) grant.Quota { return f.quota }

func TestAdmitIsAtomicAcrossConcurrentStarters(t *testing.T) {
	c := New(Options{
		ConfigDir: t.TempDir(),
		Grants:    fixedGrants{allowed: true, quota: grant.Quota{MaxInstances: 1}},
	})
	tl := &tool.Tool{ID: "demo", Resources: tool.Resources{CPU: 1, Memory: "1Gi"}}
	j := &job.Job{}

	first, err := c.admit("alice", tl, j, "alice-demo-1")
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if _, err := c.admit("alice", tl, j, "alice-demo-1"); err == nil {
		t.Fatal("a second unit was admitted while the first reservation was live")
	}
	first.Release()
	first.Release() // idempotent: failure exits call it more than once

	third, err := c.admit("alice", tl, j, "alice-demo-1")
	if err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	third.Release()
}

// Once the record exists, the reservation must not charge the same unit again:
// one running unit + one new unit fits a quota of two.
func TestReservationStopsCountingWhenTheRecordExists(t *testing.T) {
	dir := t.TempDir()
	c := New(Options{
		ConfigDir: dir,
		Grants:    fixedGrants{allowed: true, quota: grant.Quota{MaxInstances: 2, MaxCPU: 2}},
	})
	tl := &tool.Tool{ID: "demo", Resources: tool.Resources{CPU: 1, Memory: "1Gi"}}
	res, err := c.admit("alice", tl, &job.Job{}, "alice-demo-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Release()
	rec := &runtime.Instance{
		ID: "alice-demo-1", User: "alice", Tool: "demo", Kind: "task",
		State: runtime.StateRunning, RequestedCPU: 1,
	}
	if err := runtime.SaveInstance(runtime.InstancePath(dir, rec.ID), rec); err != nil {
		t.Fatal(err)
	}
	if err := c.checkQuota("alice", tl, &job.Job{}); err != nil {
		t.Fatalf("the reservation and the record were double-counted: %v", err)
	}
}

func TestAdmitRefusesAnUnauthorizedUser(t *testing.T) {
	c := New(Options{
		ConfigDir: t.TempDir(),
		Grants:    fixedGrants{allowed: false},
	})
	tl := &tool.Tool{ID: "demo", Resources: tool.Resources{CPU: 1, Memory: "1Gi"}}
	if _, err := c.admit("alice", tl, &job.Job{}, "alice-demo-1"); err == nil {
		t.Fatal("admission must refuse a user without a grant")
	}
}
