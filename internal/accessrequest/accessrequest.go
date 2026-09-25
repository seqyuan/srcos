// Package accessrequest is the tool-access request surface: a user asking for a
// tool they cannot use yet, and an administrator's decision about it (B3).
//
// A request is a file — 文件系统即数据库, the same rule as everything else
// here: data/requests/<id>.yaml. It grants nothing by itself. Approval writes a
// grant through the same grant.Policy.AddUserToGrant the CLI uses, so "why can
// alice use this tool" still points at a line someone wrote in grants.yaml,
// which is what makes the feature auditable rather than a side door.
//
// Requests live under data/ (runtime state), not config/ (declaration state):
// backing up the policy must not carry the pile of requests with it.
package accessrequest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// State is where a request stands. Pending is the only non-terminal state.
type State string

const (
	Pending  State = "pending"
	Approved State = "approved"
	Denied   State = "denied"
)

// ErrNotFound is returned when no request has that id.
var ErrNotFound = errors.New("no such request")

// ErrNotPending is returned when deciding a request that is already decided.
var ErrNotPending = errors.New("request is already decided")

// Request is one user's ask for one tool.
type Request struct {
	ID     string `yaml:"id" json:"id"`
	User   string `yaml:"user" json:"user"`
	Tool   string `yaml:"tool" json:"tool"`
	Reason string `yaml:"reason,omitempty" json:"reason,omitempty"`
	State  State  `yaml:"state" json:"state"`

	CreatedAt time.Time `yaml:"created_at" json:"createdAt"`
	// DecidedAt / DecidedBy / Note are set when the request leaves pending.
	DecidedAt time.Time `yaml:"decided_at,omitempty" json:"decidedAt,omitempty"`
	DecidedBy string    `yaml:"decided_by,omitempty" json:"decidedBy,omitempty"`
	Note      string    `yaml:"note,omitempty" json:"note,omitempty"`
}

// Terminal reports whether a decision has been made.
func (r Request) Terminal() bool { return r.State != Pending }

// Dir is where requests for a deployment live.
func Dir(dataDir string) string { return filepath.Join(dataDir, "requests") }

// Path is one request's file.
func Path(dataDir, id string) string { return filepath.Join(Dir(dataDir), id+".yaml") }

// createMu serializes the dedupe scan with the write. Requests are created only
// through the gateway (agents have no request tool, and the CLI does not create
// them), so one process is enough to make "one pending per (user, tool)" true
// rather than best-effort.
var createMu sync.Mutex

// Create files a request, or returns the caller's existing pending one.
//
// A second request for the same (user, tool) does not create a second file:
// an impatient click must not turn into a queue the administrator has to prune.
// created reports whether a new file was written.
func Create(dataDir, user, tool, reason string, now time.Time) (r Request, created bool, err error) {
	createMu.Lock()
	defer createMu.Unlock()

	if existing, ok, lerr := findPending(dataDir, user, tool); lerr != nil {
		return Request{}, false, lerr
	} else if ok {
		return existing, false, nil
	}
	r = Request{
		ID:        newID(user, tool, now),
		User:      user,
		Tool:      tool,
		Reason:    strings.TrimSpace(reason),
		State:     Pending,
		CreatedAt: now.UTC(),
	}
	if err := save(dataDir, r); err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// Decide records an administrator's decision. Only a pending request can be
// decided; deciding twice is an error, not a silent overwrite.
func Decide(dataDir, id, by string, approve bool, note string, now time.Time) (Request, error) {
	r, err := Get(dataDir, id)
	if err != nil {
		return Request{}, err
	}
	if r.Terminal() {
		return r, fmt.Errorf("%w: %s is %s", ErrNotPending, r.ID, r.State)
	}
	if approve {
		r.State = Approved
	} else {
		r.State = Denied
	}
	r.DecidedAt = now.UTC()
	r.DecidedBy = by
	r.Note = strings.TrimSpace(note)
	if err := save(dataDir, r); err != nil {
		return Request{}, err
	}
	return r, nil
}

// Get loads one request by id.
func Get(dataDir, id string) (Request, error) {
	if !ValidID(id) {
		return Request{}, ErrNotFound
	}
	data, err := os.ReadFile(Path(dataDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return Request{}, ErrNotFound
		}
		return Request{}, err
	}
	var r Request
	if err := yaml.Unmarshal(data, &r); err != nil {
		return Request{}, fmt.Errorf("%s: %w", id, err)
	}
	return r, nil
}

// List returns every request, oldest first.
func List(dataDir string) ([]Request, error) {
	entries, err := os.ReadDir(Dir(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Request
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		r, err := Get(dataDir, strings.TrimSuffix(e.Name(), ".yaml"))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// ListFor returns one user's requests, oldest first.
func ListFor(dataDir, user string) ([]Request, error) {
	all, err := List(dataDir)
	if err != nil {
		return nil, err
	}
	var out []Request
	for _, r := range all {
		if r.User == user {
			out = append(out, r)
		}
	}
	return out, nil
}

// findPending returns the caller's pending request for a tool, if any.
func findPending(dataDir, user, tool string) (Request, bool, error) {
	all, err := ListFor(dataDir, user)
	if err != nil {
		return Request{}, false, err
	}
	for _, r := range all {
		if r.Tool == tool && r.State == Pending {
			return r, true, nil
		}
	}
	return Request{}, false, nil
}

// ValidID guards the file path: an id becomes a file name, so it may not carry
// separators or surprises.
func ValidID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// newID derives a readable, filesystem-safe id: date, who, what, and a short
// random tail so two requests in the same second do not collide.
func newID(user, tool string, now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("req-%s-%s-%s-%s",
		now.UTC().Format("20060102"), slug(user), slug(tool), hex.EncodeToString(b[:]))
}

func slug(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

// save writes one request atomically (temp + rename), so a crash mid-write
// cannot leave a half-parsed decision behind.
func save(dataDir string, r Request) error {
	if err := os.MkdirAll(Dir(dataDir), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(r)
	if err != nil {
		return err
	}
	final := Path(dataDir, r.ID)
	tmp, err := os.CreateTemp(Dir(dataDir), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}
