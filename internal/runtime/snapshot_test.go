package runtime

import "testing"

// snapshotFor supplies a lifecycle consumer the record list it now takes as a
// parameter. Tests live in package runtime, so the runner's config dir is
// reachable without threading it through every test.
func snapshotFor(t *testing.T, r *Runner) []*Instance {
	t.Helper()
	insts, err := ListInstances(r.opts.ConfigDir)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	return insts
}
