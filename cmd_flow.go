package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/srcos/internal/flow"
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

	fs := newFlagSet("flow")
	configDir := configDirFlag(fs)
	flowsDir := fs.String("flows-dir", "", "flow package root (default <program dir>/srcos-flows)")
	toolsDir := fs.String("tools-dir", "", "tool package root (default <program dir>/srcos-tools)")
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos flow <list|validate> [options] [flow-id]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  list                     list flows (nodes, layers, sample columns)")
		fmt.Fprintln(os.Stderr, "  validate [flow-id]       check the graph, the wiring and the tools it references")
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

// resolveFlowsDir mirrors config.ResolveToolsDir for flows: an explicit flag,
// then $SRCOS_FLOWS_DIR, then the repository layout, then the install layout.
func resolveFlowsDir(override, configDir string) string {
	if override != "" {
		return override
	}
	if env := strings.TrimSpace(os.Getenv("SRCOS_FLOWS_DIR")); env != "" {
		return env
	}
	for _, candidate := range []string{"srcos-flows", "flows"} {
		path := filepath.Join(filepath.Dir(configDir), candidate)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(filepath.Dir(configDir), "srcos-flows")
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
