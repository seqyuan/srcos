package grant

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// Save writes the policy atomically.
//
// Atomic because the gateway watches this file: a half-written grants.yaml
// would either deny everything or expose everything for the moment it takes to
// notice, and "which of the two" is not a coin worth flipping.
func Save(path string, p *Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	header := "# SRCOS 授权策略（Grant）\n" +
		"#\n" +
		"# 默认拒绝：未被任何 grant 提到的工具对非管理员不可见。\n" +
		"# 需要全站公开就写 `default_allow: true`，或给某个工具写 `public: true`。\n" +
		"# 只表达「允许」，没有 deny —— 所以「为什么 alice 能用」永远能指到某一行。\n" +
		"#\n" +
		"# 由 `srcos grant ...` 维护；手工编辑后重启网关生效。\n\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append([]byte(header), data...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// SetGrant inserts or replaces the grant for a tool.
func (p *Policy) SetGrant(g Grant) {
	for i := range p.Grants {
		if p.Grants[i].Tool == g.Tool {
			p.Grants[i] = g
			p.sortGrants()
			return
		}
	}
	p.Grants = append(p.Grants, g)
	p.sortGrants()
}

// RemoveGrant deletes the grant for a tool and reports whether it existed.
func (p *Policy) RemoveGrant(tool string) bool {
	for i := range p.Grants {
		if p.Grants[i].Tool == tool {
			p.Grants = append(p.Grants[:i], p.Grants[i+1:]...)
			return true
		}
	}
	return false
}

// Grant returns the grant for a tool.
func (p *Policy) Grant(tool string) (Grant, bool) { return p.grantFor(tool) }

// GrantTools lists the tools with an explicit grant, sorted.
func (p *Policy) GrantTools() []string {
	out := make([]string, 0, len(p.Grants))
	for _, g := range p.Grants {
		out = append(out, g.Tool)
	}
	sort.Strings(out)
	return out
}

// SetGroup inserts or replaces a group's membership. Empty membership deletes
// the group, so `--user` with nothing to add is not left as a ghost entry.
func (p *Policy) SetGroup(name string, members []string) {
	if p.Groups == nil {
		p.Groups = map[string][]string{}
	}
	if len(members) == 0 {
		delete(p.Groups, name)
		return
	}
	sort.Strings(members)
	p.Groups[name] = members
}

// GroupNames lists group names, sorted.
func (p *Policy) GroupNames() []string {
	out := make([]string, 0, len(p.Groups))
	for name := range p.Groups {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// SetAdmins replaces the admin list.
func (p *Policy) SetAdmins(admins []string) {
	sort.Strings(admins)
	p.Admins = admins
}

// AddUserToGrant adds a user to a tool's grant, creating the grant if needed.
//
// Creating on demand is deliberate: the CLI operation a person actually wants
// is "give alice this tool", and requiring a separate "create grant" step would
// mean a half-configured state that denies what the operator just asked for.
func (p *Policy) AddUserToGrant(tool, user string) {
	g, _ := p.grantFor(tool)
	g.Tool = tool
	for _, u := range g.Users {
		if u == user {
			return
		}
	}
	g.Users = append(g.Users, user)
	sort.Strings(g.Users)
	p.SetGrant(g)
}

// AddGroupToGrant adds a group to a tool's grant.
func (p *Policy) AddGroupToGrant(tool, group string) {
	g, _ := p.grantFor(tool)
	g.Tool = tool
	for _, existing := range g.Groups {
		if existing == group {
			return
		}
	}
	g.Groups = append(g.Groups, group)
	sort.Strings(g.Groups)
	p.SetGrant(g)
}

// RemoveUserFromGrant removes a user, dropping the grant when it admits nobody.
func (p *Policy) RemoveUserFromGrant(tool, user string) bool {
	g, ok := p.grantFor(tool)
	if !ok {
		return false
	}
	kept := g.Users[:0]
	removed := false
	for _, u := range g.Users {
		if u == user {
			removed = true
			continue
		}
		kept = append(kept, u)
	}
	g.Users = kept
	if !g.Public && len(g.Users) == 0 && len(g.Groups) == 0 {
		p.RemoveGrant(tool)
		return removed
	}
	p.SetGrant(g)
	return removed
}

// RemoveGroupFromGrant removes a group, dropping the grant when it admits nobody.
func (p *Policy) RemoveGroupFromGrant(tool, group string) bool {
	g, ok := p.grantFor(tool)
	if !ok {
		return false
	}
	kept := g.Groups[:0]
	removed := false
	for _, existing := range g.Groups {
		if existing == group {
			removed = true
			continue
		}
		kept = append(kept, existing)
	}
	g.Groups = kept
	if !g.Public && len(g.Users) == 0 && len(g.Groups) == 0 {
		p.RemoveGrant(tool)
		return removed
	}
	p.SetGrant(g)
	return removed
}

// SetQuota sets a tool's quota without disturbing its membership.
func (p *Policy) SetQuota(tool string, q Quota) error {
	g, ok := p.grantFor(tool)
	if !ok {
		return fmt.Errorf("no grant for %s: grant the tool to someone first", tool)
	}
	g.Quota = q
	p.SetGrant(g)
	return nil
}

// Describe renders a policy for `srcos grant list`.
func (p *Policy) Describe() string {
	var b []byte
	add := func(format string, a ...any) { b = append(b, []byte(fmt.Sprintf(format, a...))...) }

	add("admins: %v\n", p.Admins)
	if p.DefaultAllow {
		add("default_allow: true  ⚠ 未授权的工具对所有登录用户可见\n")
	} else {
		add("default_allow: false （默认拒绝）\n")
	}
	if len(p.Groups) > 0 {
		add("\ngroups:\n")
		for _, name := range p.GroupNames() {
			add("  %-16s %v\n", name, p.Groups[name])
		}
	}
	if len(p.Grants) == 0 {
		add("\ngrants: （无）\n")
		return string(b)
	}
	add("\ngrants:\n")
	add("  %-20s %-28s %-10s %s\n", "TOOL", "WHO", "INSTANCES", "CPU / MEMORY")
	for _, g := range p.Grants {
		who := "public"
		switch {
		case g.Public && (len(g.Users) > 0 || len(g.Groups) > 0):
			who = "public + " + joinWho(g)
		case !g.Public:
			who = joinWho(g)
		}
		inst := "-"
		if g.Quota.MaxInstances > 0 {
			inst = fmt.Sprintf("%d", g.Quota.MaxInstances)
		}
		limits := "-"
		if g.Quota.MaxCPU > 0 || g.Quota.MaxMemory != "" {
			limits = fmt.Sprintf("%d / %s", g.Quota.MaxCPU, orDash(g.Quota.MaxMemory))
		}
		add("  %-20s %-28s %-10s %s\n", g.Tool, who, inst, limits)
	}
	return string(b)
}

func joinWho(g Grant) string {
	var parts []string
	for _, u := range g.Users {
		parts = append(parts, "user:"+u)
	}
	for _, grp := range g.Groups {
		parts = append(parts, "group:"+grp)
	}
	if len(parts) == 0 {
		return "-"
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "," + p
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (p *Policy) sortGrants() {
	sort.Slice(p.Grants, func(i, j int) bool { return p.Grants[i].Tool < p.Grants[j].Tool })
}
