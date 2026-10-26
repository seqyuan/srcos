package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/environment"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// doctorCheck is one deployment-level finding. "warn" means the deployment can
// serve but something is not configured; "fail" means a tool that needs it will
// fail at run time.
type doctorCheck struct {
	Name   string
	Status string // ok | warn | fail
	Detail string
}

func (c doctorCheck) failed() bool { return c.Status == "fail" }

// doctorReport is deliberately offline: it never needs a running gateway. It
// covers the things readyz cannot: writable data dirs, whether this filesystem
// can arbitrate flocks, stale legacy claim markers, per-tool sandbox binaries
// and flow package validation.
func doctorReport(configDir, toolsDir, flowsDir string) []doctorCheck {
	var checks []doctorCheck
	add := func(name, status, format string, a ...any) {
		checks = append(checks, doctorCheck{Name: name, Status: status, Detail: fmt.Sprintf(format, a...)})
	}

	if err := checkWritableDir(configDir); err != nil {
		add("config", "fail", "%v", err)
	} else {
		add("config", "ok", "%s", configDir)
	}

	dataDir := config.DataDir(configDir)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		add("data", "fail", "create %s: %v", dataDir, err)
	} else if err := checkWritableDir(dataDir); err != nil {
		add("data", "fail", "%v", err)
	} else {
		add("data", "ok", "%s", dataDir)
	}

	// The one failure mode introduced by moving claims out of the workspace:
	// a filesystem without flock makes every job unclaimable.
	if err := job.ProbeLease(filepath.Join(dataDir, "claims")); err != nil {
		add("claims", "fail", "flock does not arbitrate in %s: %v", filepath.Join(dataDir, "claims"), err)
	} else {
		add("claims", "ok", "flock arbitrates in %s", filepath.Join(dataDir, "claims"))
	}

	tools, toolsErr := tool.Discover(toolsDir)
	switch {
	case toolsErr != nil:
		if os.IsNotExist(toolsErr) {
			add("tools", "warn", "%s does not exist (no tools registered)", toolsDir)
		} else {
			add("tools", "fail", "%v", toolsErr)
		}
	default:
		add("tools", "ok", "%d tool(s) in %s", len(tools), toolsDir)
		needBwrap, needApptainer := false, false
		for _, t := range tools {
			switch t.Sandbox {
			case tool.SandboxBwrap:
				needBwrap = true
			case tool.SandboxApptainer:
				needApptainer = true
			}
		}
		if needBwrap {
			path, ok, why := sandbox.BwrapProbe()
			if ok {
				add("bwrap", "ok", "%s", path)
			} else {
				add("bwrap", "fail", "%s", why)
			}
		}
		if needApptainer {
			path, ok, why := sandbox.ApptainerProbe()
			if ok {
				add("apptainer", "ok", "%s", path)
			} else {
				add("apptainer", "fail", "%s", why)
			}
		}
	}

	provider, err := storage.Load(config.StoragesPath(configDir))
	if err != nil {
		add("storages", "fail", "%v", err)
	} else if checker, ok := provider.(storage.ReachabilityChecker); ok {
		if problems := checker.CheckReachable(); len(problems) > 0 {
			add("storages", "fail", "%s", strings.Join(problems, "; "))
		} else {
			add("storages", "ok", "%d storage(s) reachable", len(provider.List()))
		}
	} else {
		add("storages", "ok", "%d storage(s)", len(provider.List()))
	}

	envs, err := environment.Load(config.EnvironmentsPath(configDir))
	if err != nil {
		add("environments", "fail", "%v", err)
	} else if checker, ok := envs.(environment.ReachabilityChecker); ok {
		if problems := checker.CheckReachable(); len(problems) > 0 {
			add("environments", "fail", "%s", strings.Join(problems, "; "))
		} else {
			add("environments", "ok", "%d environment(s) reachable", len(envs.List()))
		}
	} else {
		add("environments", "ok", "%d environment(s)", len(envs.List()))
	}

	if _, err := os.Stat(flowsDir); err != nil {
		if os.IsNotExist(err) {
			add("flows", "warn", "%s does not exist (no flows registered)", flowsDir)
		} else {
			add("flows", "fail", "%v", err)
		}
	} else if flows, err := flow.Discover(flowsDir); err != nil {
		add("flows", "fail", "%v", err)
	} else {
		var problems []string
		for _, f := range flows {
			if err := f.ValidateAgainst(doctorToolResolver(toolsDir)); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", f.ID, err))
			}
		}
		if len(problems) > 0 {
			add("flows", "fail", "%s", strings.Join(problems, "; "))
		} else {
			add("flows", "ok", "%d flow(s) valid", len(flows))
		}
	}

	return checks
}

func doctorToolResolver(toolsDir string) func(id, version string) (*tool.Tool, error) {
	return func(id, version string) (*tool.Tool, error) {
		t, err := tool.Find(toolsDir, id)
		if err != nil {
			return nil, err
		}
		if version != "" && t.Version != version {
			return nil, fmt.Errorf("version %s not found (have %s)", version, t.Version)
		}
		return t, nil
	}
}

func doctorExitCode(checks []doctorCheck) int {
	for _, c := range checks {
		if c.failed() {
			return 1
		}
	}
	return 0
}

func runDoctor(args []string) {
	fs := newFlagSet("doctor")
	configDir := configDirFlag(fs)
	toolsDir := fs.String("tools-dir", "", "tool package root (default <program dir>/tools)")
	flowsDir := fs.String("flows-dir", "", "flow package root (default <program dir>/srcos-flows)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos doctor [-d CONFIG] [--tools-dir DIR] [--flows-dir DIR]")
		fs.PrintDefaults()
	}
	var positional []string
	parseFlagsLoose(fs, args, &positional)
	if len(positional) > 0 {
		fs.Usage()
		os.Exit(1)
	}
	checks := doctorReport(*configDir, resolveToolsDir(*toolsDir, *configDir), resolveFlowsDir(*flowsDir, *configDir))
	for _, c := range checks {
		fmt.Printf("%-4s %-14s %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
	}
	os.Exit(doctorExitCode(checks))
}
