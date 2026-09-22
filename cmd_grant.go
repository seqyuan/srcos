package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
)

// srcos grant ... maintains config/grants.yaml.
//
// Every subcommand is a read-modify-write of one file, and the file is saved
// atomically: the gateway watches it, so a partial write would either deny
// everything or expose everything for as long as it took to notice.
func runGrantCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list", "show":
		runGrantList(sub, args)
	case "set":
		runGrantSet(args)
	case "rm", "remove":
		runGrantRemove(args)
	case "group":
		runGrantGroup(args)
	case "admin":
		runGrantAdmin(args)
	case "allow":
		runGrantToggle(args, true)
	case "deny":
		runGrantToggle(args, false)
	default:
		fmt.Fprintf(os.Stderr, "unknown grant subcommand: %s\n", sub)
		printGrantUsage()
		os.Exit(1)
	}
}

func printGrantUsage() {
	fmt.Fprintln(os.Stderr, "usage: srcos grant <list|show|set|rm|group|admin|allow|deny> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  list                     show the whole policy")
	fmt.Fprintln(os.Stderr, "  show <tool>              show one tool's grant and who it admits")
	fmt.Fprintln(os.Stderr, "  set <tool> [options]     replace a tool's grant")
	fmt.Fprintln(os.Stderr, "      --user <name>         (repeatable)")
	fmt.Fprintln(os.Stderr, "      --group <name>        (repeatable, must exist)")
	fmt.Fprintln(os.Stderr, "      --public              any authenticated account")
	fmt.Fprintln(os.Stderr, "      --max-cpu <n>         aggregate cores for this user+tool")
	fmt.Fprintln(os.Stderr, "      --max-memory <size>   aggregate memory, e.g. 64Gi")
	fmt.Fprintln(os.Stderr, "      --max-instances <n>   aggregate live instances")
	fmt.Fprintln(os.Stderr, "  rm <tool>                remove a tool's grant (denies everyone but admins)")
	fmt.Fprintln(os.Stderr, "  group <name> [options]   replace a group's membership")
	fmt.Fprintln(os.Stderr, "      --user <name>         (repeatable; --delete removes the group)")
	fmt.Fprintln(os.Stderr, "  admin [--add <user>] [--rm <user>]")
	fmt.Fprintln(os.Stderr, "  allow <tool> --user <u>  add a user to a tool's grant without rewriting it")
	fmt.Fprintln(os.Stderr, "  deny  <tool> --user <u>  remove a user from a tool's grant")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Policy: deny by default. A tool no grant mentions is invisible to non-admins.")
	fmt.Fprintln(os.Stderr, "There is no \"deny\" rule — only positive statements — so \"why can alice use")
	fmt.Fprintln(os.Stderr, "this\" always points at a line someone wrote.")
}

// grantPolicy loads the policy, or an empty deny-by-default one.
func grantPolicy(configDir string) *grant.Policy {
	p, err := grant.Load(config.GrantsPath(configDir))
	if err != nil {
		fatalf("%v", err)
	}
	if p == nil {
		p, _ = grant.New(nil, nil, nil)
	}
	return p
}

func saveGrantPolicy(configDir string, p *grant.Policy) {
	if err := grant.Save(config.GrantsPath(configDir), p); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("saved %s\n", config.GrantsPath(configDir))
	fmt.Println("restart the gateway for changes to take effect")
}

func runGrantList(sub string, args []string) {
	fs := newFlagSet("grant " + sub)
	configDir := configDirFlag(fs)
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	p := grantPolicy(*configDir)

	if sub == "show" && len(positional) > 0 {
		tool := positional[0]
		g, ok := p.Grant(tool)
		if !ok {
			fmt.Printf("%s: no grant — %s\n", tool, deniedNote(p))
			return
		}
		fmt.Printf("tool      %s\n", tool)
		fmt.Printf("public    %v\n", g.Public)
		fmt.Printf("users     %v\n", g.Users)
		fmt.Printf("groups    %v\n", g.Groups)
		fmt.Printf("quota     cpu=%d memory=%s instances=%d\n", g.Quota.MaxCPU, dash(g.Quota.MaxMemory), g.Quota.MaxInstances)
		fmt.Printf("effective %v\n", whoIsAllowed(p, g))
		return
	}
	fmt.Print(p.Describe())
	if !p.DefaultAllow {
		fmt.Println("\n未授权（非管理员不可见）：" + deniedNote(p))
	}
}

func deniedNote(p *grant.Policy) string {
	if p.DefaultAllow {
		return "default_allow: true —— 所有登录用户可见"
	}
	return "默认拒绝"
}

func whoIsAllowed(p *grant.Policy, g grant.Grant) []string {
	var out []string
	if g.Public {
		out = append(out, "(everyone)")
	}
	out = append(out, g.Users...)
	for _, grp := range g.Groups {
		out = append(out, p.Groups[grp]...)
	}
	return out
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runGrantSet(args []string) {
	fs := newFlagSet("grant set")
	configDir := configDirFlag(fs)
	users := &strList{}
	groups := &strList{}
	fs.Var(users, "user", "username (repeatable)")
	fs.Var(groups, "group", "group name (repeatable)")
	public := fs.Bool("public", false, "any authenticated account")
	maxCPU := fs.Int("max-cpu", 0, "aggregate CPU cores")
	maxMemory := fs.String("max-memory", "", "aggregate memory, e.g. 64Gi")
	maxInstances := fs.Int("max-instances", 0, "aggregate live instances")
	var positional []string
	fs.Usage = func() { printGrantUsage() }
	parseFlagsLoose(fs, args, &positional)

	if len(positional) == 0 {
		printGrantUsage()
		os.Exit(1)
	}
	tool := positional[0]
	p := grantPolicy(*configDir)

	// Validate before writing: a grant naming a group that does not exist would
	// deny the very user the operator meant to admit, silently.
	for _, grp := range *groups {
		if !containsStr(p.GroupNames(), grp) {
			fatalf("unknown group %q (existing: %v); create it with `srcos grant group %s --user ...`",
				grp, p.GroupNames(), grp)
		}
	}

	g := grant.Grant{
		Tool:   tool,
		Users:  *users,
		Groups: *groups,
		Public: *public,
		Quota: grant.Quota{
			MaxCPU:       *maxCPU,
			MaxMemory:    *maxMemory,
			MaxInstances: *maxInstances,
		},
	}
	p.SetGrant(g)
	saveGrantPolicy(*configDir, p)
}

func runGrantRemove(args []string) {
	fs := newFlagSet("grant rm")
	configDir := configDirFlag(fs)
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		printGrantUsage()
		os.Exit(1)
	}
	p := grantPolicy(*configDir)
	if !p.RemoveGrant(positional[0]) {
		fatalf("no grant for %s", positional[0])
	}
	saveGrantPolicy(*configDir, p)
}

func runGrantGroup(args []string) {
	fs := newFlagSet("grant group")
	configDir := configDirFlag(fs)
	users := &strList{}
	fs.Var(users, "user", "username (repeatable)")
	del := fs.Bool("delete", false, "remove the group")
	var positional []string
	fs.Usage = func() { printGrantUsage() }
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		printGrantUsage()
		os.Exit(1)
	}
	name := positional[0]
	p := grantPolicy(*configDir)

	if *del {
		p.SetGroup(name, nil)
	} else {
		if len(*users) == 0 {
			fatalf("group %s would be empty: pass --user, or --delete to remove it", name)
		}
		p.SetGroup(name, *users)
	}
	saveGrantPolicy(*configDir, p)
}

func runGrantAdmin(args []string) {
	fs := newFlagSet("grant admin")
	configDir := configDirFlag(fs)
	add := &strList{}
	rm := &strList{}
	fs.Var(add, "add", "promote a user (repeatable)")
	fs.Var(rm, "rm", "demote a user (repeatable)")
	var positional []string
	fs.Usage = func() { printGrantUsage() }
	parseFlagsLoose(fs, args, &positional)
	_ = positional

	p := grantPolicy(*configDir)
	admins := append([]string{}, p.Admins...)
	for _, u := range *add {
		if !containsStr(admins, u) {
			admins = append(admins, u)
		}
	}
	for _, u := range *rm {
		admins = removeStr(admins, u)
	}
	if len(*add) == 0 && len(*rm) == 0 {
		fmt.Printf("admins: %v\n", p.Admins)
		return
	}
	p.SetAdmins(admins)
	saveGrantPolicy(*configDir, p)
}

// runGrantToggle adds or removes one user, without rewriting the whole grant.
// This is the operation actually typed in practice ("give alice this tool").
func runGrantToggle(args []string, allow bool) {
	name := "allow"
	if !allow {
		name = "deny"
	}
	fs := newFlagSet("grant " + name)
	configDir := configDirFlag(fs)
	users := &strList{}
	groups := &strList{}
	fs.Var(users, "user", "username (repeatable)")
	fs.Var(groups, "group", "group name (repeatable)")
	var positional []string
	fs.Usage = func() { printGrantUsage() }
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 || (len(*users) == 0 && len(*groups) == 0) {
		printGrantUsage()
		os.Exit(1)
	}
	tool := positional[0]
	p := grantPolicy(*configDir)

	changed := 0
	for _, u := range *users {
		if allow {
			p.AddUserToGrant(tool, u)
			changed++
			continue
		}
		if p.RemoveUserFromGrant(tool, u) {
			changed++
		}
	}
	for _, g := range *groups {
		if allow {
			if !containsStr(p.GroupNames(), g) {
				fatalf("unknown group %q (existing: %v)", g, p.GroupNames())
			}
			p.AddGroupToGrant(tool, g)
			changed++
			continue
		}
		if p.RemoveGroupFromGrant(tool, g) {
			changed++
		}
	}
	if changed == 0 {
		fmt.Println("nothing changed")
		return
	}
	saveGrantPolicy(*configDir, p)
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func removeStr(list []string, drop string) []string {
	out := list[:0]
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}
