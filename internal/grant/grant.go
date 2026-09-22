// Package grant is the authorization model: who may use which tool, and with
// how much.
//
// Two decisions shape it, and both are deliberate:
//
//   - **Deny by default.** A tool that no grant mentions is invisible to
//     non-admin users. The alternative — "visible unless denied" — means
//     publishing a tool publishes it to everyone, which is the wrong default
//     for a platform whose selling point is auditable, controlled execution.
//     A tool meant for everybody says so explicitly with `public: true`.
//
//   - **Positive statements only.** There is no `deny` and no negation. Access
//     is the union of what grants affirm, so the answer to "why can alice use
//     this" is always a line someone wrote, never the absence of one. That is
//     what makes the model reviewable.
//
// Quota checking is pure arithmetic over counts, so this package imports
// nothing from the runtime. The caller supplies what is currently in use; the
// policy decides whether one more fits.
package grant

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Policy is the parsed grants.yaml.
//
// It is safe for concurrent use: every request asks it a question, and the
// admin surface mutates it while those requests are in flight. Mutating it *in
// place* (rather than swapping a new value in) is what makes an authorization
// change take effect immediately — every holder of this pointer, in this process
// and in the API handlers, sees the new answer on its next request.
type Policy struct {
	// mu guards every field below. Public methods take it; the *Locked helpers
	// assume it is already held (Go's RWMutex is not reentrant, so a public
	// method must never call another public one while holding it).
	mu sync.RWMutex

	// Groups name sets of users. Membership is a list, not a role: a user can
	// be in any number of groups, and a grant may name several.
	Groups map[string][]string `yaml:"groups,omitempty"`

	// Admins bypass every grant. They manage the platform, so gating their own
	// access on a grant they could edit would be circular. They cannot, however,
	// exceed a tool's declared resource ceiling — that is a property of the
	// tool, not of the person.
	Admins []string `yaml:"admins,omitempty"`

	Grants []Grant `yaml:"grants,omitempty"`

	// DefaultAllow is the explicit opt-out for a single-user or internal
	// deployment that wants every tool visible to every account. It defaults to
	// false, and being a named field it shows up in the file a reviewer reads —
	// unlike a compile-time flag that no operator ever sees.
	DefaultAllow bool `yaml:"default_allow,omitempty"`
}

// Snapshot returns a deep copy, for callers that need a consistent view without
// holding the lock (the YAML writer, and the admin page's rendering).
func (p *Policy) Snapshot() *Policy {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := &Policy{
		Admins:       append([]string(nil), p.Admins...),
		DefaultAllow: p.DefaultAllow,
	}
	if p.Groups != nil {
		out.Groups = make(map[string][]string, len(p.Groups))
		for name, members := range p.Groups {
			out.Groups[name] = append([]string(nil), members...)
		}
	}
	for _, g := range p.Grants {
		out.Grants = append(out.Grants, g.clone())
	}
	return out
}

// ReplaceWith copies another policy's contents into this one, in place.
//
// In place, because the pointers already handed out must keep working: the API
// handlers, the page renderer and the read model all hold *this* policy, and a
// swap elsewhere would leave them answering from the old one. It is how a
// hand-edited grants.yaml takes effect without a restart.
func (p *Policy) ReplaceWith(other *Policy) {
	if p == nil || other == nil || p == other {
		return
	}
	other.mu.RLock()
	groups := make(map[string][]string, len(other.Groups))
	for name, members := range other.Groups {
		groups[name] = append([]string(nil), members...)
	}
	admins := append([]string(nil), other.Admins...)
	grants := make([]Grant, 0, len(other.Grants))
	for _, g := range other.Grants {
		grants = append(grants, g.clone())
	}
	defaultAllow := other.DefaultAllow
	other.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.Groups = groups
	p.Admins = admins
	p.Grants = grants
	p.DefaultAllow = defaultAllow
}

// clone copies a grant, so a snapshot never shares a slice with the live policy.
func (g Grant) clone() Grant {
	out := g
	out.Users = append([]string(nil), g.Users...)
	out.Groups = append([]string(nil), g.Groups...)
	return out
}

// Grant is one positive access statement.
//
// The json tags mirror the yaml ones in camelCase: the management API serves
// this structure, and a wire format that leaked Go field names (`MaxCPU`) would
// be a second, accidental contract.
type Grant struct {
	// Tool is a tool id. A single "*" entry applies to every tool that no more
	// specific grant mentions, which is the escape hatch for "this deployment
	// publishes its tools to all staff".
	Tool string `yaml:"tool" json:"tool"`

	Users  []string `yaml:"users,omitempty" json:"users,omitempty"`
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"`
	// Public grants access to every authenticated user.
	Public bool `yaml:"public,omitempty" json:"public,omitempty"`

	// Quota is this grant's own ceiling. It is applied *in addition to* the
	// tool's declared resources, so a user can never exceed what the tool is
	// willing to run with, and may be given less.
	Quota Quota `yaml:"quota,omitempty" json:"quota,omitempty"`
}

// Quota is a ceiling on one user's use of one tool.
//
// MaxInstances and the aggregate CPU/memory limits are what stop a single user
// from filling the machine by starting many instances of a small tool; the
// per-instance limits live in the tool's own `resources`.
type Quota struct {
	MaxCPU       int    `yaml:"max_cpu,omitempty" json:"maxCpu,omitempty"`
	MaxMemory    string `yaml:"max_memory,omitempty" json:"maxMemory,omitempty"`
	MaxInstances int    `yaml:"max_instances,omitempty" json:"maxInstances,omitempty"`
}

// IsZero reports whether the quota constrains nothing.
func (q Quota) IsZero() bool {
	return q.MaxCPU == 0 && q.MaxMemory == "" && q.MaxInstances == 0
}

// Usage is what a user is currently consuming, or is about to.
type Usage struct {
	Instances int
	CPU       int
	Memory    uint64 // bytes
}

// Load reads grants.yaml. A missing file yields a deny-by-default policy: a
// deployment with no grants has no tools visible to anyone, which fails loudly
// in the UI rather than silently exposing every tool.
func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(nil, nil, nil)
		}
		return nil, err
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

// New builds a policy from its parts and validates it.
func New(groups map[string][]string, admins []string, grants []Grant) (*Policy, error) {
	p := &Policy{Groups: groups, Admins: admins, Grants: grants}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Validate checks the policy's internal consistency.
func (p *Policy) Validate() error {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.validateLocked()
}

func (p *Policy) validateLocked() error {
	var problems []string
	bad := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	for name, members := range p.Groups {
		if strings.TrimSpace(name) == "" {
			bad("a group has an empty name")
		}
		seen := map[string]bool{}
		for _, u := range members {
			if strings.TrimSpace(u) == "" {
				bad("group %q lists an empty username", name)
				continue
			}
			if seen[u] {
				bad("group %q lists %s twice", name, u)
			}
			seen[u] = true
		}
	}

	seenTools := map[string]bool{}
	for i, g := range p.Grants {
		where := fmt.Sprintf("grants[%d]", i)
		if strings.TrimSpace(g.Tool) == "" {
			bad("%s: tool is required (use \"*\" for a catch-all)", where)
			continue
		}
		where = fmt.Sprintf("grant for %s", g.Tool)
		if seenTools[g.Tool] {
			// Two entries for one tool would make the effective quota depend on
			// order, which is exactly the kind of ambiguity this model avoids.
			bad("%s: duplicate grant (merge the two into one entry)", where)
		}
		seenTools[g.Tool] = true

		if !g.Public && len(g.Users) == 0 && len(g.Groups) == 0 {
			bad("%s: grants nobody — add users, groups, or `public: true`", where)
		}
		for _, grp := range g.Groups {
			if _, ok := p.Groups[grp]; !ok {
				bad("%s: unknown group %q", where, grp)
			}
		}
		if g.Quota.MaxCPU < 0 || g.Quota.MaxInstances < 0 {
			bad("%s: quota values must not be negative", where)
		}
		if g.Quota.MaxMemory != "" {
			if _, err := parseMemory(g.Quota.MaxMemory); err != nil {
				bad("%s: quota.max_memory: %v", where, err)
			}
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("%d problem(s):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

// Allowed reports whether a user may use a tool.
//
// Resolution order: admins always; then the tool's own grant; then the "*"
// catch-all. A tool with neither is denied.
func (p *Policy) Allowed(username, toolID string) bool {
	if p == nil {
		// No policy configured at all. The caller decides what that means; in
		// practice the server substitutes a deny-all policy, so reaching here
		// with nil means "authorization is not wired", and allowing is the
		// honest answer for a single-user deployment.
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.isAdminLocked(username) {
		return true
	}
	if p.DefaultAllow {
		return true
	}
	if g, ok := p.grantForLocked(toolID); ok && g.Allows(username, p.Groups) {
		return true
	}
	if g, ok := p.grantForLocked("*"); ok && g.Allows(username, p.Groups) {
		return true
	}
	return false
}

// IsAdmin reports whether a user is an administrator.
func (p *Policy) IsAdmin(username string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isAdminLocked(username)
}

func (p *Policy) isAdminLocked(username string) bool {
	for _, a := range p.Admins {
		if a == username {
			return true
		}
	}
	return false
}

// QuotaFor returns the effective quota for a user and tool.
//
// The tool's own grant wins over the "*" catch-all; if neither sets a quota the
// result is the zero value, which CheckQuota treats as "no limit beyond the
// tool's own declaration".
func (p *Policy) QuotaFor(username, toolID string) Quota {
	if p == nil {
		return Quota{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if g, ok := p.grantForLocked(toolID); ok && g.Allows(username, p.Groups) {
		return g.Quota
	}
	if g, ok := p.grantForLocked("*"); ok && g.Allows(username, p.Groups) {
		return g.Quota
	}
	return Quota{}
}

// ReachesAnyone reports whether at least one non-admin could reach a tool.
//
// It exists for the startup warning: the symptom of a missing grant is a user
// seeing an empty catalogue, not an error, so the operator should hear about it
// first. It deliberately does not answer "who": that requires enumerating users,
// which the policy does not know.
func (p *Policy) ReachesAnyone(toolID string) bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.DefaultAllow {
		return true
	}
	for _, g := range []string{toolID, "*"} {
		entry, ok := p.grantForLocked(g)
		if !ok {
			continue
		}
		if entry.Public || len(entry.Users) > 0 || len(entry.Groups) > 0 {
			return true
		}
	}
	return false
}

// ToolsNobodyCanReach lists tools that have an explicit grant admitting nobody.
// A tool with *no* grant is not listed, because that is the common case and the
// caller already reports those separately.
func (p *Policy) ToolsNobodyCanReach() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for _, g := range p.Grants {
		if !g.Public && len(g.Users) == 0 && len(g.Groups) == 0 {
			out = append(out, g.Tool)
		}
	}
	sort.Strings(out)
	return out
}

// GroupsOf returns the groups a user belongs to, for display and audit.
func (p *Policy) GroupsOf(username string) []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for name, members := range p.Groups {
		for _, m := range members {
			if m == username {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// grantForLocked returns a copy of a tool's grant. Callers hold the lock.
func (p *Policy) grantForLocked(toolID string) (Grant, bool) {
	for _, g := range p.Grants {
		if g.Tool == toolID {
			return g.clone(), true
		}
	}
	return Grant{}, false
}

// Allows reports whether this grant admits a user.
func (g Grant) Allows(username string, groups map[string][]string) bool {
	if g.Public {
		return true
	}
	for _, u := range g.Users {
		if u == username {
			return true
		}
	}
	for _, grp := range g.Groups {
		for _, m := range groups[grp] {
			if m == username {
				return true
			}
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────
// 配额
// ─────────────────────────────────────────────────────────────────────────

// CheckQuota reports whether adding `want` to `used` stays inside q.
//
// It returns a message aimed at the person who hit the limit, not at a
// developer: the point of an aggregate quota is that it is invisible until it
// bites, so when it bites it has to explain itself.
func CheckQuota(q Quota, used, want Usage) error {
	if q.IsZero() {
		return nil
	}
	if q.MaxInstances > 0 && used.Instances+want.Instances > q.MaxInstances {
		return fmt.Errorf("instance quota reached: you have %d of %d allowed for this tool; stop one first",
			used.Instances, q.MaxInstances)
	}
	if q.MaxCPU > 0 && used.CPU+want.CPU > q.MaxCPU {
		if used.CPU == 0 {
			return fmt.Errorf("this request needs %d cores but your quota is %d", want.CPU, q.MaxCPU)
		}
		return fmt.Errorf("CPU quota reached: %d of %d cores in use, this request needs %d more",
			used.CPU, q.MaxCPU, want.CPU)
	}
	if q.MaxMemory != "" {
		limit, err := parseMemory(q.MaxMemory)
		if err == nil && used.Memory+want.Memory > limit {
			if used.Memory == 0 {
				return fmt.Errorf("this request needs %s of memory but your quota is %s",
					formatMemory(want.Memory), q.MaxMemory)
			}
			return fmt.Errorf("memory quota reached: %s of %s in use, this request needs %s more",
				formatMemory(used.Memory), q.MaxMemory, formatMemory(want.Memory))
		}
	}
	return nil
}

// parseMemory accepts the same suffix grammar as tool.yaml, duplicated here to
// keep this package dependency-free. The grammar is small and frozen; a shared
// helper would create an import that the policy does not otherwise need.
func parseMemory(s string) (uint64, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   uint64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12},
		{"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12},
	}
	for _, u := range units {
		if !strings.HasSuffix(v, u.suffix) {
			continue
		}
		num := strings.TrimSpace(strings.TrimSuffix(v, u.suffix))
		n, err := parseFloat(num)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid memory %q", s)
		}
		return uint64(n * float64(u.mult)), nil
	}
	n, err := parseUint(v)
	if err != nil {
		return 0, fmt.Errorf("invalid memory %q (want e.g. 32Gi, 512M, or bytes)", s)
	}
	return n, nil
}

func formatMemory(b uint64) string {
	if b == 0 {
		return "0"
	}
	for _, u := range []struct {
		suffix string
		mult   uint64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if b%u.mult == 0 {
			return fmt.Sprintf("%d%s", b/u.mult, u.suffix)
		}
	}
	return fmt.Sprintf("%d", b)
}

func parseFloat(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

func parseUint(s string) (uint64, error) { return strconv.ParseUint(s, 10, 64) }
