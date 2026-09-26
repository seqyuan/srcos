package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/tool"
)

// srcos audit ... reads the structured audit stream (data/audit/audit-*.jsonl).
//
// This is the read half of the audit surface: the gateway and the CLI write it
// (who / when / which tool version / what params / allowed or denied), and this
// command answers "what happened" without grepping a log file.
func runAuditCmd(args []string) {
	sub := "tail"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "tail":
		runAuditTail(args)
	case "list", "ls":
		runAuditList(args)
	case "prune":
		runAuditPrune(args)
	case "verify":
		runAuditVerify(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown audit subcommand: %s\n", sub)
		printAuditUsage()
		os.Exit(1)
	}
}

func printAuditUsage() {
	fmt.Fprintln(os.Stderr, "usage: srcos audit <tail|list> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  tail [-n 20]             the most recent events, oldest first")
	fmt.Fprintln(os.Stderr, "  list                     all matching events (filter below)")
	fmt.Fprintln(os.Stderr, "      --user <name>        only this user's acts")
	fmt.Fprintln(os.Stderr, "      --action <name>      submit | cancel | run_flow | login | csrf | scope | ...")
	fmt.Fprintln(os.Stderr, "      --decision <d>       allow | deny")
	fmt.Fprintln(os.Stderr, "      --since <when>       24h | 7d | 2h30m | RFC3339 (default: all)")
	fmt.Fprintln(os.Stderr, "      --limit <n>          keep only the newest n after filtering")
	fmt.Fprintln(os.Stderr, "      --json               raw JSON, one event per line")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Events live in data/audit/audit-YYYY-MM-DD.jsonl (append-only, 0600).")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  prune --keep 90d         remove whole day-files older than the retention window")
	fmt.Fprintln(os.Stderr, "      --dry-run            show what would be removed")
	fmt.Fprintln(os.Stderr, "  verify                   check the per-file hash chain (non-zero exit if broken)")
}

func runAuditVerify(args []string) {
	fs := newFlagSet("audit verify")
	configDir := configDirFlag(fs)
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	problems, err := audit.Verify(config.DataDir(*configDir))
	if err != nil {
		fatalf("%v", err)
	}
	if len(problems) == 0 {
		fmt.Println("ok: every chained line verifies")
		return
	}
	for _, p := range problems {
		where := p.File
		if p.Line > 0 {
			where = fmt.Sprintf("%s:%d", p.File, p.Line)
		}
		fmt.Printf("%s  %s\n", where, p.Reason)
	}
	fmt.Printf("\n%d problem(s)\n", len(problems))
	os.Exit(1)
}

func runAuditPrune(args []string) {
	fs := newFlagSet("audit prune")
	configDir := configDirFlag(fs)
	keep := fs.String("keep", "90d", "retention window (e.g. 90d, 2160h); 0 removes nothing")
	dryRun := fs.Bool("dry-run", false, "show what would be removed")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	keepDur, err := audit.ParseKeep(*keep)
	if err != nil {
		fatalf("--keep: %v", err)
	}
	dataDir := config.DataDir(*configDir)

	if *dryRun {
		stale, err := audit.Stale(dataDir, keepDur, time.Now())
		if err != nil {
			fatalf("%v", err)
		}
		if len(stale) == 0 {
			fmt.Println("nothing older than " + *keep)
			return
		}
		for _, path := range stale {
			fmt.Println(filepath.Base(path))
		}
		return
	}

	removed, err := audit.Prune(dataDir, keepDur, time.Now())
	if err != nil {
		fatalf("%v", err)
	}
	if len(removed) == 0 {
		fmt.Println("nothing to prune (keep=" + *keep + ")")
		return
	}
	for _, name := range removed {
		fmt.Println("removed", name)
	}

	// Deleting records is itself an audited act, so the stream says who pruned
	// it and how much went.
	rec := audit.New(dataDir)
	rec.Record(audit.NewEvent(operatorActor(), "audit.prune").
		WithParams(map[string]any{"keep": *keep, "files": len(removed)}).
		WithRefs(map[string]string{"oldest": removed[0], "newest": removed[len(removed)-1]}).
		Allowed())
	rec.Close()
	fmt.Printf("pruned %d file(s); recorded as audit.prune\n", len(removed))
}

func runAuditTail(args []string) {
	fs := newFlagSet("audit tail")
	configDir := configDirFlag(fs)
	n := fs.Int("n", 20, "number of events")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	events, err := audit.Tail(config.DataDir(*configDir), audit.Filter{}, *n)
	if err != nil {
		fatalf("%v", err)
	}
	printAuditEvents(events, false)
}

func runAuditList(args []string) {
	fs := newFlagSet("audit list")
	configDir := configDirFlag(fs)
	user := fs.String("user", "", "filter by user")
	action := fs.String("action", "", "filter by action")
	decision := fs.String("decision", "", "allow | deny")
	since := fs.String("since", "", "e.g. 24h, 7d, or an RFC3339 timestamp")
	limit := fs.Int("limit", 0, "keep only the newest n after filtering")
	asJSON := fs.Bool("json", false, "raw JSON lines")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	filter := audit.Filter{User: *user, Action: *action, Decision: *decision}
	if *since != "" {
		at, err := audit.ParseSince(*since)
		if err != nil {
			fatalf("--since: %v", err)
		}
		filter.Since = at
	}
	events, err := audit.Query(config.DataDir(*configDir), filter)
	if err != nil {
		fatalf("%v", err)
	}
	if *limit > 0 && len(events) > *limit {
		events = events[len(events)-*limit:]
	}
	printAuditEvents(events, *asJSON)
}

func printAuditEvents(events []audit.Event, asJSON bool) {
	if len(events) == 0 {
		fmt.Println("no audit events")
		return
	}
	for _, e := range events {
		if asJSON {
			data, err := json.Marshal(e)
			if err != nil {
				fatalf("%v", err)
			}
			fmt.Println(string(data))
			continue
		}
		fmt.Println(formatAuditEvent(e))
	}
}

func formatAuditEvent(e audit.Event) string {
	var b strings.Builder
	b.WriteString(e.TS.Format(time.RFC3339))
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%-5s", e.Decision))
	b.WriteString("  ")
	b.WriteString(e.Action)
	b.WriteString("  ")
	b.WriteString(auditActor(e.Actor))
	if e.Target.ID != "" {
		b.WriteString("  ")
		b.WriteString(e.Target.Type)
		b.WriteString(" ")
		b.WriteString(e.Target.ID)
		if e.Target.Version != "" {
			b.WriteString("@" + e.Target.Version)
		}
		if e.Target.Digest != "" {
			b.WriteString("#" + tool.ShortDigest(e.Target.Digest))
		}
	}
	for _, k := range sortedKeys(e.Refs) {
		b.WriteString("  " + k + "=" + e.Refs[k])
	}
	if e.Reason != "" {
		b.WriteString("  reason: " + e.Reason)
	}
	if len(e.Params) > 0 {
		var ps []string
		for _, k := range sortedKeys(e.Params) {
			ps = append(ps, k+"="+e.Params[k])
		}
		b.WriteString("  {" + strings.Join(ps, " ") + "}")
	}
	if e.Outcome != nil && !e.Outcome.OK && e.Outcome.Error != "" {
		b.WriteString("  error: " + e.Outcome.Error)
	}
	return b.String()
}

func auditActor(a audit.Actor) string {
	who := a.User
	if who == "" {
		who = "anonymous"
	}
	switch a.Kind {
	case audit.KindAgentToken:
		who += " (agent_token"
		if a.TokenID != "" {
			who += " " + a.TokenID
		}
		if a.Label != "" {
			who += " " + a.Label
		}
		who += ")"
	case audit.KindCLI:
		who += " (cli)"
	case audit.KindSystem:
		who += " (system)"
	}
	return who
}

func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// cliAudit records a configuration change made from the command line. The
// gateway's own mutations are recorded by the API handlers; this is the other
// door into the same files, and an audit trail that omitted it would have a
// hole shaped exactly like `srcos grant set`.
func cliAudit(configDir string, actor audit.Actor, action, targetType, targetID string, params map[string]any) {
	rec := audit.New(config.DataDir(configDir))
	defer rec.Close()
	rec.Record(audit.NewEvent(actor, action).
		WithTarget(targetType, targetID, "").
		WithParams(params).
		Allowed())
}

// operatorActor is who ran the CLI: the OS account, the only identity a local
// command has.
func operatorActor() audit.Actor {
	return audit.Actor{User: operatorName(), Kind: audit.KindCLI}
}

// operatorName is who ran the CLI: the OS account, which is the only identity a
// local command has.
func operatorName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "cli"
}
