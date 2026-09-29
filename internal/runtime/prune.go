package runtime

import (
	"fmt"
	"os"
	"time"
)

// Instance records are history, and history grows without bound: every task and
// every service ever run leaves a YAML file under data/instances, and the scan
// tick lists all of them several times a minute. Nothing deletes them on its
// own, so without this the list (and the cost of reading it) grows forever.
//
// Pruning is deliberately explicit and never automatic, mirroring `audit
// prune`: how long a lab keeps its run history is an operator's decision, not a
// side effect of a background tick. Outputs and workspaces are never touched —
// they are the user's data, not the platform's bookkeeping.

// StaleInstances returns terminal instance records whose end time is older than
// the retention window, without removing anything.
//
// Only terminal records are candidates: a running or queued instance is live
// runtime state, not history — deleting its record would orphan a process and
// free its quota behind the platform's back.
func StaleInstances(configDir string, keep time.Duration, now time.Time) ([]*Instance, error) {
	if keep <= 0 {
		return nil, nil
	}
	insts, err := ListInstances(configDir)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-keep)
	var stale []*Instance
	for _, inst := range insts {
		if !inst.State.Terminal() {
			continue
		}
		if instanceEndedAt(inst).Before(cutoff) {
			stale = append(stale, inst)
		}
	}
	return stale, nil
}

// PruneInstances removes terminal instance records older than the retention
// window, along with each record's log and systemd verdict file. It returns
// what it removed.
//
// A record that was already removed between the listing and the delete is not
// an error (another process may have pruned it), so this is safe to re-run.
func PruneInstances(configDir string, keep time.Duration, now time.Time) ([]*Instance, error) {
	stale, err := StaleInstances(configDir, keep, now)
	if err != nil {
		return nil, err
	}
	var removed []*Instance
	for _, inst := range stale {
		for _, path := range instanceArtifacts(configDir, inst) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("remove %s: %w", path, err)
			}
		}
		removed = append(removed, inst)
	}
	return removed, nil
}

// instanceEndedAt is when a record left the live set: EndedAt when the runner
// wrote it, falling back to StartedAt for an older record that never got one.
func instanceEndedAt(inst *Instance) time.Time {
	if !inst.EndedAt.IsZero() {
		return inst.EndedAt
	}
	return inst.StartedAt
}

// instanceArtifacts lists the files that belong to one instance record: the
// record itself, its log, and the verdict systemd writes beside the log.
func instanceArtifacts(configDir string, inst *Instance) []string {
	paths := []string{InstancePath(configDir, inst.ID)}
	if inst.LogPath != "" {
		paths = append(paths, inst.LogPath, inst.LogPath+".verdict")
	}
	return paths
}
