package agenttoken

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// flushEvery throttles writes per token. "Last used" is a hint for a
// revocation decision ("is this token still in use?"), not an audit log —
// losing the last few seconds of it to a crash is a good trade for not writing
// a file on every agent request.
const flushEvery = 30 * time.Second

// Usage records when each token was last used.
//
// It lives in data/ rather than config/ on purpose: `srcos token create` runs
// as a separate process while the gateway may be answering requests, and a
// gateway write into agent-tokens.yaml could clobber a token the operator just
// created. Two files, two writers, no interleaving.
//
// This is deliberately *not* the audit log (that is a separate Phase 3 item):
// it keeps one timestamp per token, and a 30-second window of it can be lost.
//
// Writes are throttled per token: the *first* use of a token is visible to
// `srcos token list` immediately (which is the moment an operator cares —
// "is this credential in use?"), while a token hammered in a loop writes at
// most once per flushEvery.
type Usage struct {
	path string

	mu      sync.Mutex
	last    map[string]time.Time
	flushed map[string]time.Time
	dirty   bool
}

// usageFile is the on-disk shape of data/agent-token-usage.yaml.
type usageFile struct {
	Usage map[string]time.Time `yaml:"usage"`
}

// LoadUsage reads the usage file, tolerating every kind of damage: an
// unreadable or malformed file means "no known usage", which must never be a
// reason to refuse a request.
func LoadUsage(path string) *Usage {
	u := &Usage{path: path, last: map[string]time.Time{}, flushed: map[string]time.Time{}}
	data, err := os.ReadFile(path)
	if err != nil {
		return u
	}
	var f usageFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		log.Printf("[srcos] %s: %v (last-used times restart from empty)", path, err)
		return u
	}
	for id, at := range f.Usage {
		if !at.IsZero() {
			u.last[id] = at
		}
	}
	return u
}

// Last returns when a token was last used, or the zero time if unknown.
func (u *Usage) Last(id string) time.Time {
	if u == nil {
		return time.Time{}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last[id]
}

// Touch records a use, writing the file when this token has not been flushed
// recently. It returns an error only when a write was attempted and failed.
func (u *Usage) Touch(id string, at time.Time) error {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	if at.After(u.last[id]) {
		u.last[id] = at
	}
	u.dirty = true
	due := time.Since(u.flushed[id]) >= flushEvery
	u.mu.Unlock()

	if !due {
		return nil
	}
	return u.Flush()
}

// Flush writes the usage file if anything changed since the last write.
func (u *Usage) Flush() error {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.dirty {
		return nil
	}

	snapshot := make(map[string]time.Time, len(u.last))
	for id, at := range u.last {
		snapshot[id] = at.UTC().Truncate(time.Second)
	}
	data, err := yaml.Marshal(usageFile{Usage: snapshot})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(u.path), 0o755); err != nil {
		return err
	}
	tmp := u.path + ".tmp"
	body := append([]byte("# SRCOS agent token usage（运行态，由网关写入）\n"+
		"# 记录每个 token 最近一次被使用的时间；`srcos token list` 会读它。\n"+
		"# 不是审计日志：只有时间戳，写入按 token 降频（首次使用即时落盘），\n"+
		"# 已撤销 token 的旧记录可能残留（按 id 合并，无害）。\n\n"), data...)
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, u.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", u.path, err)
	}
	u.dirty = false
	now := time.Now()
	for id := range u.last {
		u.flushed[id] = now
	}
	return nil
}
