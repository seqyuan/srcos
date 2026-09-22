// Package activity is a write-behind journal of timestamps: "when was this id
// last used".
//
// Two parts of SRCOS need exactly that, and neither belongs in the other's
// package: the agent-token surface needs "when was this token last used" (for
// `srcos token list`), and the service reaper needs "is this instance still
// being used" (idleTTL must not kill a notebook someone is working in). Both
// are high-frequency reads of a *low-value* fact, so both want the same
// mechanics: keep it in memory, write it behind a throttle, and never let a
// failed write turn into a failed request.
//
// The journal lives in data/ rather than inside the records it describes. That
// is deliberate: an instance record is written by whoever starts or stops the
// unit, which may be another process, and a heartbeat that rewrote the record
// could clobber a state change with a stale "running". One writer per file.
package activity

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// flushEvery throttles writes per id. The first use of an id is written
// immediately — that is the moment a reader cares ("is this credential in
// use?" / "has anything touched this service?") — and after that one write per
// id per window is plenty for a hint that only ever drives a minutes-scale
// decision.
const flushEvery = 30 * time.Second

// Journal is the in-memory map plus its file.
type Journal struct {
	path   string
	header string

	mu      sync.Mutex
	last    map[string]time.Time
	flushed map[string]time.Time
	dirty   bool
}

// file is the on-disk shape: a flat id → timestamp map, so a reader of the
// file (a human, or another process's reaper) does not need a schema.
type file struct {
	Entries map[string]time.Time `yaml:"entries"`
}

// Load reads a journal, tolerating every kind of damage: an unreadable or
// malformed file means "no recorded activity", which must never be a reason to
// refuse a request or to reap something.
func Load(path, header string) *Journal {
	j := &Journal{
		path:    path,
		header:  header,
		last:    map[string]time.Time{},
		flushed: map[string]time.Time{},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return j
	}
	var f file
	if err := yaml.Unmarshal(data, &f); err != nil {
		log.Printf("[srcos] %s: %v (activity times restart from empty)", path, err)
		return j
	}
	for id, at := range f.Entries {
		if !at.IsZero() {
			j.last[id] = at
		}
	}
	return j
}

// Last returns when an id was last used, or the zero time if unknown.
func (j *Journal) Last(id string) time.Time {
	if j == nil {
		return time.Time{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last[id]
}

// Touch records a use, writing the file when this id has not been written
// recently. It returns an error only when a write was attempted and failed.
func (j *Journal) Touch(id string, at time.Time) error {
	if j == nil || id == "" {
		return nil
	}
	j.mu.Lock()
	if at.After(j.last[id]) {
		j.last[id] = at
	}
	j.dirty = true
	due := time.Since(j.flushed[id]) >= flushEvery
	j.mu.Unlock()

	if !due {
		return nil
	}
	return j.Flush()
}

// Flush writes the journal if anything changed since the last write.
func (j *Journal) Flush() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.dirty {
		return nil
	}

	snapshot := make(map[string]time.Time, len(j.last))
	for id, at := range j.last {
		snapshot[id] = at.UTC().Truncate(time.Second)
	}
	data, err := yaml.Marshal(file{Entries: snapshot})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	tmp := j.path + ".tmp"
	body := append([]byte(j.header), data...)
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", j.path, err)
	}
	j.dirty = false
	now := time.Now()
	for id := range j.last {
		j.flushed[id] = now
	}
	return nil
}
