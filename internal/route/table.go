// Package route is the dynamic routing table: the single coupling point
// between the orchestration layer and the proxy layer.
//
// AGENTS.md requires that the orchestration layer never touches HTTP and the
// proxy layer never touches containers. They meet here: a service instance
// publishes an endpoint, the proxy reads it before falling back to the static
// card list in config/users/<user>.yaml.
//
// Nothing user-supplied reaches a value in this table unchecked: an endpoint is
// only ever a loopback port handed out by the port pool, and the frontend path
// is derived from the authenticated user plus the tool id. That is what keeps a
// table consulted on every proxied request from becoming an SSRF surface.
package route

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Target is a live backend endpoint.
type Target struct {
	// Host must be a loopback address. The proxy re-validates through
	// config.SafeDialContext, but a table entry is still written defensively.
	Host string
	Port int
}

// String renders host:port for dialing.
func (t Target) String() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// Entry is one dynamic route.
type Entry struct {
	User       string
	Tool       string
	InstanceID string
	// Path is the frontend prefix, e.g. "/proxy/alice/jupyter". It never ends
	// in a slash, matching how the static card paths are written.
	Path      string
	Target    Target
	WebSocket bool
	BWLimit   int64
	// BackendPath mirrors the static service option of the same name: the
	// prefix the backend itself expects.
	BackendPath string
	// State is informational; the proxy only serves entries whose target is
	// dialable.
	State string
}

// Key derives the table key: one live instance per (user, tool).
func Key(user, tool string) string { return user + "\x00" + tool }

// Table is a concurrency-safe map from (user, tool) to a live endpoint.
//
// It is deliberately in-memory: instance records live in YAML on disk and are
// replayed at startup, so the table is a cache of "what is reachable right
// now", not a source of truth.
type Table struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

func NewTable() *Table {
	return &Table{entries: map[string]Entry{}}
}

// Put installs or replaces a route.
func (t *Table) Put(e Entry) error {
	if e.User == "" || e.Tool == "" {
		return fmt.Errorf("route needs a user and a tool")
	}
	if e.InstanceID == "" {
		return fmt.Errorf("route for %s/%s needs an instance id", e.User, e.Tool)
	}
	if e.Target.Host == "" {
		e.Target.Host = "127.0.0.1"
	}
	if !isLoopback(e.Target.Host) {
		return fmt.Errorf("route target %q must be a loopback address: service instances listen on 127.0.0.1 only", e.Target.Host)
	}
	if e.Target.Port <= 0 || e.Target.Port > 65535 {
		return fmt.Errorf("route target port %d is out of range", e.Target.Port)
	}
	if e.Path == "" {
		e.Path = DefaultPath(e.User, e.Tool)
	}
	if !strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("route path %q must be absolute", e.Path)
	}
	e.Path = strings.TrimSuffix(e.Path, "/")

	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[Key(e.User, e.Tool)] = e
	return nil
}

// Get looks up a route by user and tool.
func (t *Table) Get(user, tool string) (Entry, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.entries[Key(user, tool)]
	return e, ok
}

// GetByPath finds a route by its frontend prefix. The comparison is on segment
// boundaries so "/proxy/alice/jupyterx" does not match "/proxy/alice/jupyter".
func (t *Table) GetByPath(path string) (Entry, string, bool) {
	path = strings.TrimSuffix(path, "/")
	t.mu.RLock()
	defer t.mu.RUnlock()

	var best Entry
	var rest string
	found := false
	for _, e := range t.entries {
		if path != e.Path && !strings.HasPrefix(path, e.Path+"/") {
			continue
		}
		// Longest prefix wins, in case one tool path is a prefix of another.
		if found && len(e.Path) <= len(best.Path) {
			continue
		}
		best = e
		rest = strings.TrimPrefix(path, e.Path)
		found = true
	}
	return best, rest, found
}

// Delete removes a route. It reports whether something was removed, so a
// caller can distinguish "instance stopped" from "instance was never routed".
func (t *Table) Delete(user, tool string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := Key(user, tool)
	if _, ok := t.entries[k]; !ok {
		return false
	}
	delete(t.entries, k)
	return true
}

// DeleteInstance removes a route only if it belongs to the given instance.
// This is the reaper's guard against deleting a successor's route.
func (t *Table) DeleteInstance(user, tool, instanceID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := Key(user, tool)
	e, ok := t.entries[k]
	if !ok || e.InstanceID != instanceID {
		return false
	}
	delete(t.entries, k)
	return true
}

// List returns every route, ordered by path for stable output.
func (t *Table) List() []Entry {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].InstanceID < out[j].InstanceID
	})
	return out
}

// Len reports how many routes are live.
func (t *Table) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.entries)
}

// ForUser returns the routes belonging to one user.
func (t *Table) ForUser(user string) []Entry {
	var out []Entry
	for _, e := range t.List() {
		if e.User == user {
			out = append(out, e)
		}
	}
	return out
}

// DefaultPath is the frontend prefix for a tool instance. Tool ids are already
// constrained to [a-z0-9-], and usernames to a leading letter plus
// alphanumerics/underscore/hyphen, so the result cannot contain a path
// separator or "..".
func DefaultPath(user, tool string) string {
	return "/proxy/" + user + "/" + tool
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
