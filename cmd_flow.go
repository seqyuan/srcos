package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/flowrun"
	"github.com/seqyuan/srcos/internal/tool"
)

// srcos flow ... inspects the orchestration contract (docs/flow-spec.md).
//
// For now that means the static half: the graph, the wiring, and the tools the
// nodes reference. Running a flow (the DAG scheduler) is Phase 4's next slice,
// and `flow validate` is what makes it safe to write one before it exists.
func runFlowCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	switch sub {
	case "run":
		runFlowRun(args)
		return
	case "resume":
		runFlowResume(args)
		return
	case "status":
		runFlowStatus(args)
		return
	case "cancel":
		runFlowCancel(args)
		return
	}

	fs := newFlagSet("flow")
	configDir := configDirFlag(fs)
	flowsDir := fs.String("flows-dir", "", "flow package root (default <program dir>/srcos-flows)")
	toolsDir := fs.String("tools-dir", "", "tool package root (default <program dir>/srcos-tools)")
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow <list|validate|run|resume|status> [options] [flow-id]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  list                     list flows (nodes, layers, sample columns)")
		fmt.Fprintln(os.Stderr, "  validate [flow-id]       check the graph, the wiring and the tools it references")
		fmt.Fprintln(os.Stderr, "  run <flow-id> --samples <file> [--param k=v] [--dry-run]")
		fmt.Fprintln(os.Stderr, "  resume <run-id>          continue a run (skips signed/succeeded nodes)")
		fmt.Fprintln(os.Stderr, "  status [run-id]          show this user's flow runs")
		fmt.Fprintln(os.Stderr, "  cancel <run-id>          stop scheduling and stop the jobs in flight")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Contract: docs/flow-spec.md")
	}
	parseFlagsLoose(fs, args, &positional)

	flowsRoot := resolveFlowsDir(*flowsDir, *configDir)
	toolsRoot := resolveToolsDir(*toolsDir, *configDir)
	resolve := toolResolver(toolsRoot)

	switch sub {
	case "list":
		flows, err := flow.Discover(flowsRoot)
		if err != nil {
			fatalf("%v", err)
		}
		if len(flows) == 0 {
			fmt.Printf("no flows found under %s\n", flowsRoot)
			return
		}
		fmt.Printf("%-20s %-8s %-7s %-6s %s\n", "ID", "VERSION", "NODES", "LAYERS", "SAMPLES")
		for _, f := range flows {
			samples := "-"
			if fields := f.SampleFields(); len(fields) > 0 {
				samples = strings.Join(fields, ",")
			}
			fmt.Printf("%-20s %-8s %-7d %-6d %s\n", f.ID, f.Version, len(f.Nodes), f.Depth(), samples)
		}

	case "validate":
		targets := []*flow.Flow{}
		if len(positional) > 0 {
			f, err := flow.Find(flowsRoot, positional[0])
			if err != nil {
				fatalf("%v", err)
			}
			targets = append(targets, f)
		} else {
			flows, err := flow.Discover(flowsRoot)
			if err != nil {
				fatalf("%v", err)
			}
			if len(flows) == 0 {
				fmt.Printf("no flows found under %s\n", flowsRoot)
				return
			}
			targets = flows
		}
		for _, f := range targets {
			if err := f.ValidateAgainst(resolve); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			printFlowSummary(f)
		}
		fmt.Printf("\n%d flow(s) valid under %s\n", len(targets), flowsRoot)

	default:
		fmt.Fprintf(os.Stderr, "unknown flow subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: srcos flow <list|validate> [options] [flow-id]")
		os.Exit(1)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// srcos flow run / resume / status
// ─────────────────────────────────────────────────────────────────────────

// flowRunnerArgs are the flags the three executing subcommands share.
type flowRunnerArgs struct {
	configDir   *string
	flowsDir    *string
	toolsDir    *string
	user        *string
	params      *kvList
	samples     *string
	dryRun      *bool
	runID       *string
	concurrency *int
}

func registerFlowRunnerFlags(fs *flag.FlagSet) *flowRunnerArgs {
	a := &flowRunnerArgs{}
	a.configDir = configDirFlag(fs)
	a.flowsDir = fs.String("flows-dir", "", "flow package root")
	a.toolsDir = fs.String("tools-dir", "", "tool package root")
	a.user = fs.String("user", "", "SRCOS registered user")
	a.params = &kvList{}
	fs.Var(a.params, "param", "value for an exposed input (repeatable: --param k=v)")
	a.samples = fs.String("samples", "", "sample table (CSV/TSV with a header row)")
	a.dryRun = fs.Bool("dry-run", false, "print what would run and submit nothing")
	a.runID = fs.String("run", "", "run id (default: generated)")
	a.concurrency = fs.Int("concurrency", flowrun.DefaultConcurrency,
		"how many jobs may run at once, across the whole run")
	return a
}

// buildFlowRunner wires the executor the same way `job run` does: one runner,
// the same sandbox, the same limits.
func buildFlowRunner(a *flowRunnerArgs) (*flowrun.Runner, string, string) {
	flowsRoot := resolveFlowsDir(*a.flowsDir, *a.configDir)
	toolsRoot := resolveToolsDir(*a.toolsDir, *a.configDir)
	user := resolveUser(*a.user)
	runner, _, err := buildRunner(*a.configDir, toolsRoot, user)
	if err != nil {
		fatalf("%v", err)
	}
	policy := grantPolicy(*a.configDir)
	if a.concurrency != nil && *a.concurrency < 1 {
		fatalf("--concurrency must be at least 1")
	}
	concurrency := flowrun.DefaultConcurrency
	if a.concurrency != nil {
		concurrency = *a.concurrency
	}
	return flowrun.New(flowrun.Options{
		ConfigDir:   *a.configDir,
		DataDir:     config.DataDir(*a.configDir),
		ToolsDir:    toolsRoot,
		FlowsDir:    flowsRoot,
		User:        user,
		Runner:      runner,
		Policy:      policy,
		Concurrency: concurrency,
		Log:         func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}), flowsRoot, user
}

func flowParams(a *flowRunnerArgs) map[string]string {
	out := map[string]string{}
	for _, kv := range *a.params {
		out[kv.key] = kv.value
	}
	return out
}

func runFlowRun(args []string) {
	fs := newFlagSet("flow run")
	a := registerFlowRunnerFlags(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow run <flow-id> --samples <file> [--param k=v] [--dry-run]")
		fs.PrintDefaults()
	}
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		printFlowRunUsage()
		os.Exit(1)
	}
	if strings.TrimSpace(*a.samples) == "" && !*a.dryRun {
		fatalf("--samples is required (a table with a header row; the flow's sample columns: see `srcos flow validate`)")
	}

	ex, flowsRoot, user := buildFlowRunner(a)
	f, err := flow.Find(flowsRoot, positional[0])
	if err != nil {
		fatalf("%v", err)
	}
	if err := f.ValidateAgainst(toolResolver(resolveToolsDir(*a.toolsDir, *a.configDir))); err != nil {
		fatalf("%v", err)
	}

	// A dry run still needs a table to expand; without one it reports what the
	// flow would need.
	var samples *flow.Samples
	if strings.TrimSpace(*a.samples) == "" {
		fmt.Printf("flow %s: %d node(s); sample columns %v; user params %v\n",
			f.ID, len(f.Nodes), f.SampleFields(), f.UserParamNames())
		fmt.Println("pass --samples <file> to see the expansion")
		return
	}
	samples, err = flow.LoadSamples(*a.samples)
	if err != nil {
		fatalf("%v", err)
	}

	runID := *a.runID
	units, _, err := ex.Plan(f, samples, flowParams(a), runID)
	if err != nil {
		fatalf("%v", err)
	}
	if runID == "" {
		runID = units[0].Tags["run"]
	}

	if *a.dryRun {
		fmt.Printf("flow %s@%s · run %s · %d sample(s) → %d job(s)\n", f.ID, f.Version, runID, len(samples.Rows), len(units))
		for _, nodeID := range flow.NodeIDs(units) {
			fmt.Printf("  %s\n", nodeID)
			for _, u := range flow.UnitsForNode(units, nodeID) {
				fmt.Printf("    %-16s params %s\n", u.Segment, renderParams(u.Params))
				if len(u.Outputs) > 0 {
					fmt.Printf("    %-16s outputs %v\n", "", u.Outputs)
				}
			}
		}
		fmt.Println("(dry run: nothing was submitted)")
		return
	}

	record := &flow.Run{
		FlowID: f.ID, FlowVersion: f.Version, User: user, ID: runID,
		Params: flowParams(a), State: flow.RunRunning, StartedAt: time.Now().UTC(),
	}
	runDir := flow.RunDir(config.DataDir(*a.configDir), user, runID)
	fmt.Printf("flow %s@%s · run %s · %d sample(s) → %d job(s)\n", f.ID, f.Version, runID, len(samples.Rows), len(units))
	fmt.Printf("run dir %s\n", runDir)
	if err := ex.Run(context.Background(), f, units, record, samples, runDir); err != nil {
		fatalf("%v", err)
	}
	fmt.Println(record.Summary())
	if record.State != flow.RunSucceeded {
		os.Exit(1)
	}
}

func printFlowRunUsage() {
	fmt.Fprintln(os.Stderr, "usage: srcos flow run <flow-id> --samples <file> [--param k=v] [--dry-run] [--run <id>]")
	fmt.Fprintln(os.Stderr, "       srcos flow resume <run-id>")
	fmt.Fprintln(os.Stderr, "       srcos flow status [run-id]")
}

func runFlowResume(args []string) {
	fs := newFlagSet("flow resume")
	a := registerFlowRunnerFlags(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow resume <run-id> [--flows-dir] [--tools-dir]")
		fs.PrintDefaults()
	}
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		printFlowRunUsage()
		os.Exit(1)
	}
	runID := positional[0]

	ex, flowsRoot, user := buildFlowRunner(a)
	dataDir := config.DataDir(*a.configDir)
	record, err := flowrun.LoadRunRecord(dataDir, user, runID)
	if err != nil {
		fatalf("%v", err)
	}
	f, err := flow.Find(flowsRoot, record.FlowID)
	if err != nil {
		fatalf("%v", err)
	}
	if err := f.ValidateAgainst(toolResolver(resolveToolsDir(*a.toolsDir, *a.configDir))); err != nil {
		fatalf("%v", err)
	}

	// The table was archived with the run, so a resume needs nothing but the id.
	samplesPath := flow.RunDir(dataDir, user, runID) + "/" + flow.SamplesCopyName
	samples, err := flow.LoadSamples(samplesPath)
	if err != nil {
		fatalf("the run's archived sample table is unreadable: %v", err)
	}
	units, _, err := ex.Plan(f, samples, record.Params, runID)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("resuming run %s (flow %s@%s)\n", runID, f.ID, f.Version)
	if err := ex.Run(context.Background(), f, units, record, samples, flow.RunDir(dataDir, user, runID)); err != nil {
		fatalf("%v", err)
	}
	fmt.Println(record.Summary())
	if record.State != flow.RunSucceeded {
		os.Exit(1)
	}
}

func runFlowStatus(args []string) {
	fs := newFlagSet("flow status")
	a := registerFlowRunnerFlags(fs)
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow status [run-id]")
		fs.PrintDefaults()
	}
	parseFlagsLoose(fs, args, &positional)

	_, _, user := buildFlowRunner(a)
	dataDir := config.DataDir(*a.configDir)
	if len(positional) > 0 {
		record, err := flowrun.LoadRunRecord(dataDir, user, positional[0])
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Println(record.Summary())
		return
	}
	records, err := flowrun.ListRunRecords(dataDir, user)
	if err != nil {
		fatalf("%v", err)
	}
	if len(records) == 0 {
		fmt.Printf("no flow runs for %s under %s\n", user, flow.RunsDir(dataDir, user))
		return
	}
	fmt.Printf("%-34s %-18s %-10s %s\n", "RUN", "FLOW", "STATE", "NODES")
	for _, rec := range records {
		done := 0
		for _, n := range rec.Nodes {
			if n.State == flow.NodeSucceeded {
				done++
			}
		}
		fmt.Printf("%-34s %-18s %-10s %d/%d done\n", rec.ID, rec.FlowID, rec.State, done, len(rec.Nodes))
	}
}

// runFlowCancel stops a run: it asks the scheduler to stop launching, stops the
// jobs already in flight, and marks the record cancelled.
//
// Order matters. The flag goes first (so the scheduler cannot launch another
// unit between the read and the stop), then the in-flight jobs are stopped, then
// the record is marked — so a crash halfway leaves the run marked "running" but
// with its flag present, which a resume would immediately honour.
func runFlowCancel(args []string) {
	fs := newFlagSet("flow cancel")
	a := registerFlowRunnerFlags(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow cancel <run-id>")
		fs.PrintDefaults()
	}
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		printFlowRunUsage()
		os.Exit(1)
	}
	runID := positional[0]

	ex, flowsRoot, user := buildFlowRunner(a)
	dataDir := config.DataDir(*a.configDir)
	record, err := flowrun.LoadRunRecord(dataDir, user, runID)
	if err != nil {
		fatalf("%v", err)
	}
	f, err := flow.Find(flowsRoot, record.FlowID)
	if err != nil {
		fatalf("%v", err)
	}

	// 1. The flag: the running scheduler notices it between jobs.
	if err := flow.RequestCancel(flow.CancelPath(dataDir, user, runID)); err != nil {
		fatalf("could not write the cancel flag: %v", err)
	}
	// 2. The jobs already in flight (read from disk by StopRun: the scheduler
	// may be another process).
	stopped, serr := ex.StopRun(context.Background(), f, runID)
	if serr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", serr)
	}
	// 3. The record, if nothing else will mark it.
	if record.State == flow.RunRunning {
		record.State = flow.RunCancelled
		record.EndedAt = time.Now().UTC()
		if err := flow.SaveRun(flow.RecordPath(dataDir, user, runID), record); err != nil {
			fatalf("%v", err)
		}
	}
	fmt.Printf("cancelled %s (%d job(s) stopped); no new jobs will be submitted\n", runID, stopped)
}

// renderParams prints a unit's params on one line, stably ordered.
func renderParams(params map[string]any) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	return strings.Join(parts, " ")
}

// printFlowSummary is the human-readable proof that a flow wires up: the layers
// it will run in, and each node's resolved tool.
func printFlowSummary(f *flow.Flow) {
	fmt.Printf("ok   %-20s v%-8s %d node(s), %d layer(s)\n", f.ID, f.Version, len(f.Nodes), f.Depth())
	for _, n := range f.Order() {
		when := ""
		if n.WhenOr() == flow.WhenAlways {
			when = "  when=always"
		}
		retry := ""
		if n.RetryMax() > 0 {
			retry = fmt.Sprintf("  retry≤%d", n.RetryMax())
		}
		deps := ""
		if len(n.DependsOn) > 0 {
			deps = "  ← " + strings.Join(n.DependsOn, ",")
		}
		fmt.Printf("     %-14s tool %s%s%s%s\n", n.ID, n.Tool, deps, when, retry)
	}
	if fields := f.SampleFields(); len(fields) > 0 {
		fmt.Printf("     sample table columns: %s\n", strings.Join(fields, ", "))
	}
}

// resolveFlowsDir is the flag override on top of config.ResolveFlowsDir, so the
// CLI, the gateway and the editor all look in the same place.
func resolveFlowsDir(override, configDir string) string {
	if override != "" {
		return override
	}
	return config.ResolveFlowsDir(configDir)
}

// toolResolver builds the lookup a flow validation needs: a tool by id, and —
// when the flow pinned a version — an exact-match check.
//
// A version mismatch is an error rather than a fallback: silently running a
// different version is how "the same flow produced different results" becomes
// unexplainable.
func toolResolver(toolsRoot string) func(id, version string) (*tool.Tool, error) {
	return func(id, version string) (*tool.Tool, error) {
		t, err := tool.Find(toolsRoot, id)
		if err != nil {
			return nil, fmt.Errorf("unknown tool %s (looked under %s)", id, toolsRoot)
		}
		if version != "" && t.Version != version {
			return nil, fmt.Errorf("tool %s is version %s here, but the flow asks for %s", id, t.Version, version)
		}
		return t, nil
	}
}
