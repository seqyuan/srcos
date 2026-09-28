package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/activity"
	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/environment"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/runtime/sge"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// ─────────────────────────────────────────────────────────────────────────
// srcos tool ...
// ─────────────────────────────────────────────────────────────────────────

func runToolCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	fs := newFlagSet("tool")
	configDir := configDirFlag(fs)
	toolsDir := fs.String("tools-dir", "", "tool package root (default <program dir>/tools)")
	var positional []string
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: srcos tool <list|validate> [options] [dir]") }
	parseFlagsLoose(fs, args, &positional)

	root := resolveToolsDir(*toolsDir, *configDir)
	// The environment provider answers "is this tool's `environment:` declared
	// here" — a declaration check, so it works on a host that cannot run that
	// environment (CI has no R). Whether *this* machine can run it is a warning.
	envPath := config.EnvironmentsPath(*configDir)
	envs, err := environment.Load(envPath)
	if err != nil {
		fatalf("%v", err)
	}

	switch sub {
	case "list":
		tools, err := tool.Discover(root)
		if err != nil {
			fatalf("%v", err)
		}
		if len(tools) == 0 {
			fmt.Printf("no tools found under %s\n", root)
			return
		}
		fmt.Printf("%-20s %-8s %-12s %-8s %-7s %-9s %s\n", "ID", "VERSION", "DIGEST", "KIND", "SANDBOX", "BACKEND", "NAME")
		for _, t := range tools {
			fmt.Printf("%-20s %-8s %-12s %-8s %-7s %-9s %s\n",
				t.ID, t.Version, tool.ShortDigest(t.Digest), t.Kind, t.Sandbox, t.Backend, t.Name)
		}

	case "validate":
		if len(positional) > 0 {
			validateOneTool(positional[0], envs, envPath)
			return
		}
		tools, err := tool.Discover(root)
		if err != nil {
			fatalf("%v", err)
		}
		if len(tools) == 0 {
			fmt.Printf("no tools found under %s\n", root)
			return
		}
		for _, t := range tools {
			validateOneTool(t.Dir, envs, envPath)
		}
		fmt.Printf("\n%d tool(s) valid under %s\n", len(tools), root)

	default:
		fmt.Fprintf(os.Stderr, "unknown tool subcommand: %s\n", sub)
		os.Exit(1)
	}
}

// validateOneTool loads a tool and reports its contract plus whether the
// declared sandbox can actually run on this host.
func validateOneTool(dir string, envs environment.Provider, envPath string) {
	t, err := tool.Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("ok   %-20s v%-8s %-8s backend=%-6s sandbox=%-10s\n",
		t.ID, t.Version, t.Kind, t.Backend, t.Sandbox)
	if t.Entry != "" {
		fmt.Printf("     entry    %s\n", t.Entry)
	} else {
		fmt.Printf("     command  %s\n", strings.Join(t.Command, " "))
	}
	// The version is a string someone wrote; the digest is what makes "which
	// code ran" answerable when the files change without it.
	fmt.Printf("     digest   %s\n", t.ShortDigestOrDash())
	fmt.Printf("     inputs   %d, outputs %d, storages %v\n",
		len(t.Interface.Inputs), len(t.Interface.Outputs), t.RequiresStorages)

	path, ok, why := sandboxStatus(t)
	if ok {
		if path != "" {
			fmt.Printf("     sandbox  ready (%s)\n", path)
		} else {
			fmt.Printf("     sandbox  ready (%s)\n", why)
		}
	} else {
		fmt.Printf("     sandbox  NOT usable: %s\n", why)
	}

	// The named environment: referencing one that is not declared is a
	// declaration-level error (host-independent, so CI catches it); not being
	// reachable on this host is a warning.
	if t.Environment == "" {
		return
	}
	e, ok := envs.Get(t.Environment)
	if !ok {
		fmt.Fprintf(os.Stderr, "     environment  %s: NOT declared in %s\n", t.Environment, envPath)
		fmt.Fprintf(os.Stderr, "\u2192 copy config/environments.example.yaml to %s and declare it there\n", envPath)
		os.Exit(1)
	}
	fmt.Printf("     environment  %s (%s)\n", e.ID, e.Root)
	for _, problem := range environment.CheckOne(e) {
		fmt.Printf("     \u26a0  %s\n", problem)
	}
}

// sandboxStatus reports whether the declared sandbox can run here, returning
// the resolved binary path, whether it works, and why not otherwise.
func sandboxStatus(t *tool.Tool) (path string, ok bool, why string) {
	switch t.Sandbox {
	case tool.SandboxNone:
		return "", true, "degraded: no namespace isolation"
	case tool.SandboxBwrap:
		return sandbox.BwrapProbe()
	case tool.SandboxApptainer:
		return "", false, "not implemented yet (Phase 6)"
	}
	return "", false, "unknown sandbox " + string(t.Sandbox)
}

// ─────────────────────────────────────────────────────────────────────────
// srcos job ...
// ─────────────────────────────────────────────────────────────────────────

func runJobCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "submit":
		runJobSubmit(args)
	case "run":
		runJobRun(args)
	case "list":
		runJobList(args)
	case "status":
		runJobStatus(args)
	case "logs":
		runJobLogs(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown job subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: srcos job <submit|run|list|status|logs> [options]")
		os.Exit(1)
	}
}

// jobSubmitFlags holds the shared option surface for submit/run/list.
type jobFlags struct {
	configDir *string
	toolsDir  *string
	user      *string
	toolID    *string
	name      *string
	cpu       *int
	mem       *string
	walltime  *string
	params    *kvList
	tags      *kvList
	outputs   *strList
	sandbox   *string
	force     *bool
	jobID     *string
}

func newJobFlags(fs *flag.FlagSet) *jobFlags {
	return &jobFlags{
		configDir: configDirFlag(fs),
		toolsDir:  fs.String("tools-dir", "", "tool package root (default <program dir>/tools)"),
		user:      fs.String("user", "", "SRCOS registered user (default: $USER)"),
		toolID:    fs.String("tool", "", "tool id (required)"),
		name:      fs.String("n", "", "job display name (default: tool id + timestamp)"),
		cpu:       fs.Int("cpu", 0, "CPU cores (may only lower the tool ceiling)"),
		mem:       fs.String("mem", "", "memory, e.g. 5Gi (may only lower the tool ceiling)"),
		walltime:  fs.String("time", "", "walltime H:MM:SS (may only lower the tool ceiling)"),
		params:    &kvList{},
		tags:      &kvList{},
		outputs:   &strList{},
		sandbox:   fs.String("sandbox", "", "override the tool's sandbox (debug aid: none|bwrap)"),
		force:     fs.Bool("force", false, "run even if a successful instance already exists"),
		jobID:     fs.String("job", "", "run only this job id"),
	}
}

func (jf *jobFlags) register(fs *flag.FlagSet) {
	fs.Var(jf.params, "param", "interface input as key=value (repeatable)")
	fs.Var(jf.tags, "tag", "task tag as key=value (repeatable)")
	fs.Var(jf.outputs, "output", "declared output sandbox path (repeatable)")
}

func runJobSubmit(args []string) {
	fs := newFlagSet("job submit")
	jf := newJobFlags(fs)
	jf.register(fs)
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos job submit -n NAME --tool ID [options] [work.sh]")
		fs.PrintDefaults()
	}
	parseFlagsLoose(fs, args, &positional)

	t, root := mustLoadTool(*jf.toolsDir, *jf.configDir, *jf.toolID)
	user := resolveUser(*jf.user)
	if err := checkGrant(*jf.configDir, user, t.ID); err != nil {
		fatalf("%v", err)
	}

	j := &job.Job{
		SchemaVersion: 1,
		Name:          *jf.name,
		Params:        map[string]any{},
		Tags:          map[string]string{},
		Outputs:       []string{},
	}
	if j.Name == "" {
		j.Name = fmt.Sprintf("%s @ %s", t.ID, time.Now().Format("2006-01-02 15:04:05"))
	}
	for _, kv := range *jf.params {
		j.Params[kv.key] = kv.value
	}
	for _, kv := range *jf.tags {
		j.Tags[kv.key] = kv.value
	}
	j.Outputs = append(j.Outputs, *jf.outputs...)
	if *jf.cpu > 0 || *jf.mem != "" || *jf.walltime != "" {
		j.Resources = &tool.Resources{CPU: *jf.cpu, Memory: *jf.mem, Walltime: *jf.walltime}
	}

	if err := job.Validate(j, t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Materialize the submission. The directory is the queue (ADR-004), and
	// the same helper backs POST /api/jobs so the two entry points cannot
	// drift on validation or layout.
	jobID, dir, err := job.Submit(*jf.configDir, user, t.ID, j)
	if err != nil {
		fatalf("%v", err)
	}
	// An explicit script argument means the caller generated this run's
	// work.sh; otherwise the tool package's entry is used at run time.
	if len(positional) > 0 {
		data, rerr := os.ReadFile(positional[0])
		if rerr != nil {
			fatalf("read %s: %v", positional[0], rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, "work.sh"), data, 0o755); werr != nil {
			fatalf("%v", werr)
		}
		if j.Command == nil {
			j.Command = []string{"bash", "work.sh"}
			// Re-write job.json so the recorded command matches the script we
			// just placed next to it.
			if _, _, err := job.Submit(*jf.configDir, user, t.ID, j); err != nil {
				fatalf("%v", err)
			}
		}
	}

	fmt.Printf("submitted %s\n", jobID)
	// The CLI submission is the other door into the drop-box, so it is recorded
	// too: the actor is the account the submission belongs to (the CLI may act
	// as --user), and the operator's OS name is the cli kind.
	cliAudit(*jf.configDir, audit.Actor{User: user, Kind: audit.KindCLI}, "submit", "tool", t.ID, map[string]any{
		"version": t.Version,
		"job":     jobID,
	})
	fmt.Printf("  tool    %s v%s (%s)\n", t.ID, t.Version, t.Kind)
	fmt.Printf("  user    %s\n", user)
	fmt.Printf("  dir     %s\n", dir)
	fmt.Printf("  params  %v\n", j.Params)
	if len(positional) > 0 {
		fmt.Printf("  script  %s (copied to work.sh)\n", positional[0])
	} else {
		fmt.Printf("  script  tool entry: %s\n", t.Entry)
	}
	fmt.Printf("\nnext: srcos job run --tool %s --tools-dir %s\n", t.ID, root)
}

func runJobRun(args []string) {
	fs := newFlagSet("job run")
	jf := newJobFlags(fs)
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos job run --tool ID [--job JOBID] [--force] [--sandbox none|bwrap]")
		fs.PrintDefaults()
	}
	parseFlagsLoose(fs, args, &positional)

	t, root := mustLoadTool(*jf.toolsDir, *jf.configDir, *jf.toolID)
	user := resolveUser(*jf.user)
	if *jf.sandbox != "" {
		// Debug aid: exercise the degraded path without editing tool.yaml.
		// Deliberately CLI-only so a job.json can never change its own sandbox.
		t.Sandbox = tool.Sandbox(*jf.sandbox)
	}
	if t.Kind != tool.KindTask {
		fatalf("tool %s is kind %s; use `srcos svc start` for services", t.ID, t.Kind)
	}

	runner, _, err := buildRunner(*jf.configDir, root, user, nil)
	if err != nil {
		fatalf("%v", err)
	}

	jobsDir := config.JobsDir(*jf.configDir, user, t.ID)
	scan, err := job.Scan(jobsDir)
	if err != nil {
		fatalf("%v", err)
	}
	for _, b := range scan.Broken {
		fmt.Fprintf(os.Stderr, "warning: broken submission ignored: %s\n", b)
	}
	if len(scan.Jobs) == 0 {
		fmt.Printf("no submissions under %s\n", jobsDir)
		return
	}

	ran := 0
	for _, loaded := range scan.Jobs {
		if *jf.jobID != "" && !jobIDMatches(loaded.ID, *jf.jobID) {
			continue
		}
		if err := job.Validate(loaded.Job, t); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", loaded.ID, err)
			continue
		}

		instID := runtime.InstanceID(user, t.ID, loaded.ID)
		recordPath := runtime.InstancePath(*jf.configDir, instID)
		prev, prevErr := runtime.LoadInstance(recordPath)
		if !*jf.force {
			if prevErr == nil && (prev.State == runtime.StateSucceeded || prev.State == runtime.StateRunning) {
				fmt.Printf("skip %s (already %s; use --force to re-run)\n", loaded.ID, prev.State)
				continue
			}
			// A claim means a runner took it — the gateway's task loop, or another
			// `job run`. Skipping is only right while that run is still coming: a
			// *terminal* record means it is over, so this invocation may retry it
			// (which is what running `job run` on a failed job has always meant).
			if job.Claimed(loaded.Dir) && (prevErr != nil || prev.State == runtime.StatePending) {
				fmt.Printf("skip %s (a runner already has it in flight; use --force to run it anyway)\n", loaded.ID)
				continue
			}
		}
		// Take the job, so the gateway's task loop does not start it too.
		if _, cerr := job.Claim(loaded.Dir); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not claim %s: %v\n", loaded.ID, cerr)
		}

		fmt.Printf("\n=== running %s (%s) ===\n", loaded.ID, loaded.Job.Name)
		inst, err := runner.RunTask(context.Background(), t, loaded)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		ran++
		fmt.Printf("--- %s: %s", inst.State, inst.Duration)
		if inst.Error != "" {
			fmt.Printf("  (%s)", inst.Error)
		}
		fmt.Println()
		if inst.Sandbox == string(tool.SandboxBwrap) && inst.Limiter == "none" {
			fmt.Println("    note: no resource limiter available — CPU/memory limits were NOT applied")
		}
	}

	if ran == 0 {
		fmt.Println("\nnothing to run")
	}
}

// buildRunner assembles the runtime for this host: the local backend, the
// shared port pool, the dynamic routing table and the storage provider.
//
// Everything a unit needs to be reachable or to see shared data is wired here,
// in one place, so no code path can build a runner that silently lacks one of
// them.
// checkGrant enforces the policy on the CLI paths that create instances on
// behalf of a user.
//
// The CLI is the operator's interface, and admins bypass the policy — but
// `srcos svc start --user alice` is still "alice uses this tool", so it is
// checked rather than assumed. Consistency between the HTTP surface and the CLI
// is what makes the audit trail trustworthy.
func checkGrant(configDir, user, toolID string) error {
	policy, err := grant.Load(config.GrantsPath(configDir))
	if err != nil {
		return err
	}
	if policy == nil {
		return nil
	}
	if !policy.Allowed(user, toolID) {
		return fmt.Errorf("user %s is not authorized for tool %s%s", user, toolID,
			grantHint(policy, user))
	}
	return nil
}

func grantHint(policy *grant.Policy, user string) string {
	if groups := policy.GroupsOf(user); len(groups) > 0 {
		return fmt.Sprintf(" (their groups: %v)", groups)
	}
	return ""
}

// buildRunner assembles a Runner (and the routing table it publishes into).
//
// auditRec may be nil: a CLI invocation gets its own recorder for the config
// directory, so the lifecycle decisions it makes (`job run`, `svc stop`,
// `svc reap`) land in the same stream as the gateway's.
func buildRunner(configDir, toolsDir, user string, auditRec *audit.Recorder) (*runtime.Runner, *route.Table, error) {
	storages, err := storage.Load(config.StoragesPath(configDir))
	if err != nil {
		return nil, nil, err
	}
	if checker, ok := storages.(storage.ReachabilityChecker); ok {
		for _, problem := range checker.CheckReachable() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", problem)
		}
	}

	// Named environments (interpreter / dependency trees). A tool that
	// references one which is not declared here fails at start; whether this
	// host can actually run it is a warning, like storages.
	environments, err := environment.Load(config.EnvironmentsPath(configDir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if checker, ok := environments.(environment.ReachabilityChecker); ok {
		for _, problem := range checker.CheckReachable() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", problem)
		}
	}

	if auditRec == nil {
		auditRec = audit.New(config.DataDir(configDir))
	}
	// Instance credentials (A1) live in a runtime file: the gateway mints and
	// revokes them as instances start and stop.
	runtimeTokens := agenttoken.New(config.InstanceTokensPath(configDir))
	if err := runtimeTokens.Reload(); err != nil {
		log.Printf("[srcos] %v — rejecting instance credentials until it is fixed", err)
	}
	ports := portpool.New(0, 0)
	routes := route.NewTable()
	backends := map[string]runtime.Backend{
		"local":    &runtime.Local{},
		"external": &runtime.External{},
	}
	// The scheduler, when the site declares one. It shares the port pool so a
	// service's ssh tunnel cannot collide with a local service's port.
	if sgeBackend, ok, serr := sge.FromStateFile(configDir); serr != nil {
		fmt.Fprintf(os.Stderr, "warning: sge backend disabled: %v\n", serr)
	} else if ok {
		sgeBackend.Config.Ports = ports
		backends["sge"] = sgeBackend
	}
	runner := runtime.NewRunner(runtime.Options{
		ConfigDir:    configDir,
		ToolsDir:     toolsDir,
		User:         user,
		Storages:     storages,
		Environments: environments,
		Routes:       routes,
		Audit:        auditRec,
		AgentTokens:  runtimeTokens,
		Backends:     backends,
	})
	runner.SetPorts(ports)
	return runner, routes, nil
}

// ─────────────────────────────────────────────────────────────────────────
// srcos svc ...
// ─────────────────────────────────────────────────────────────────────────

func runSvcCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "start":
		runSvcStart(args)
	case "stop":
		runSvcStop(args)
	case "list":
		runSvcList(args)
	case "reconcile":
		runSvcReconcile(args)
	case "reap":
		runSvcReap(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown svc subcommand: %s\n", sub)
		fmt.Fprintln(os.Stderr, "usage: srcos svc <start|stop|list|reconcile|reap> [options]")
		os.Exit(1)
	}
}

func runSvcStart(args []string) {
	fs := newFlagSet("svc start")
	jf := newJobFlags(fs)
	jf.register(fs)
	var positional []string
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: srcos svc start --tool ID [--param k=v ...] [--sandbox none|bwrap]")
		fs.PrintDefaults()
	}
	parseFlagsLoose(fs, args, &positional)

	t, root := mustLoadTool(*jf.toolsDir, *jf.configDir, *jf.toolID)
	user := resolveUser(*jf.user)
	if *jf.sandbox != "" {
		t.Sandbox = tool.Sandbox(*jf.sandbox)
	}
	if t.Kind != tool.KindService {
		fatalf("tool %s is kind %s; use `srcos job run` for tasks", t.ID, t.Kind)
	}
	if err := checkGrant(*jf.configDir, user, t.ID); err != nil {
		fatalf("%v", err)
	}

	j := serviceJob(t, *jf.name, *jf.params, *jf.tags)
	if err := job.Validate(j, t); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	runner, routes, err := buildRunner(*jf.configDir, root, user, nil)
	if err != nil {
		fatalf("%v", err)
	}

	// An existing live instance is replaced, not leaked: starting twice should
	// converge on one service, matching "one live instance per (user, tool)".
	if prev, err := runtime.LoadInstance(runtime.InstancePath(*jf.configDir, runtime.InstanceID(user, t.ID, ""))); err == nil {
		if !prev.State.Terminal() {
			fmt.Printf("stopping the previous instance (%s)\n", prev.State)
			_ = runner.StopService(context.Background(), t, prev)
		}
	}

	inst, err := runner.StartService(context.Background(), t, j)
	if err != nil {
		fatalf("%v", err)
	}
	if inst.State != runtime.StateRunning {
		printInstance(inst)
		os.Exit(1)
	}

	if err := runtime.WriteServiceManifest(*jf.configDir, user, t.ID, j); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist the service parameters: %v\n", err)
	}

	printInstance(inst)
	fmt.Printf("\nroute    http://<gateway>/%s\n", strings.TrimPrefix(inst.RoutePath, "/"))
	if n := routes.Len(); n > 0 {
		fmt.Printf("routes   %d live\n", n)
	}
}

func runSvcStop(args []string) {
	fs := newFlagSet("svc stop")
	configDir := configDirFlag(fs)
	toolsDir := fs.String("tools-dir", "", "tool package root")
	userFlag := fs.String("user", "", "SRCOS registered user")
	toolID := fs.String("tool", "", "tool id (required)")
	var positional []string
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: srcos svc stop --tool ID") }
	parseFlagsLoose(fs, args, &positional)

	t, root := mustLoadTool(*toolsDir, *configDir, *toolID)
	user := resolveUser(*userFlag)
	runner, _, err := buildRunner(*configDir, root, user, nil)
	if err != nil {
		fatalf("%v", err)
	}
	inst, err := runtime.LoadInstance(runtime.InstancePath(*configDir, runtime.InstanceID(user, t.ID, "")))
	if err != nil {
		fatalf("no service instance for %s/%s: %v", user, t.ID, err)
	}
	if inst.State.Terminal() {
		fmt.Printf("already %s\n", inst.State)
		return
	}
	if err := runner.StopService(context.Background(), t, inst); err != nil {
		fatalf("%v", err)
	}
	cliAudit(*configDir, operatorActor(), "instance.stopped", "instance", inst.ID, map[string]any{
		"tool":  inst.Tool,
		"owner": inst.User,
	})
	_ = runtime.WriteServiceManifest(*configDir, user, t.ID, nil)
	fmt.Printf("stopped %s\n", inst.ID)
}

func runSvcList(args []string) {
	fs := newFlagSet("svc list")
	configDir := configDirFlag(fs)
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	insts, err := runtime.ListInstances(*configDir)
	if err != nil {
		fatalf("%v", err)
	}
	printed := 0
	fmt.Printf("%-34s %-11s %-22s %-9s %s\n", "INSTANCE", "STATE", "ENDPOINT", "SANDBOX", "ROUTE")
	for _, i := range insts {
		if i.Kind != string(tool.KindService) {
			continue
		}
		printed++
		fmt.Printf("%-34s %-11s %-22s %-9s %s\n", i.ID, i.State, i.Endpoint, i.Sandbox, i.RoutePath)
	}
	if printed == 0 {
		fmt.Println("no service instances recorded")
	}
}

func runSvcReconcile(args []string) {
	fs := newFlagSet("svc reconcile")
	configDir := configDirFlag(fs)
	toolsDir := fs.String("tools-dir", "", "tool package root")
	userFlag := fs.String("user", "", "SRCOS registered user")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	user := resolveUser(*userFlag)
	runner, routes, err := buildRunner(*configDir, resolveToolsDir(*toolsDir, *configDir), user, nil)
	if err != nil {
		fatalf("%v", err)
	}
	adopted, orphaned, err := runner.Reconcile(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	fmt.Printf("adopted  %d %v\n", len(adopted), adopted)
	fmt.Printf("orphaned %d %v\n", len(orphaned), orphaned)
	fmt.Printf("routes   %d live\n", routes.Len())
}

func runSvcReap(args []string) {
	fs := newFlagSet("svc reap")
	configDir := configDirFlag(fs)
	toolsDir := fs.String("tools-dir", "", "tool package root")
	userFlag := fs.String("user", "", "SRCOS registered user")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	user := resolveUser(*userFlag)
	runner, _, err := buildRunner(*configDir, resolveToolsDir(*toolsDir, *configDir), user, nil)
	if err != nil {
		fatalf("%v", err)
	}
	// The proxy's heartbeat is what makes idleTTL mean "no traffic" rather than
	// "started a while ago": read the same journal the gateway writes.
	activityJournal := activity.Load(config.ServiceActivityPath(*configDir), "")
	reaper := &runtime.Reaper{
		Runner:     runner,
		LastActive: activityJournal.Last,
	}
	stopped, err := reaper.Sweep(context.Background(), time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}
	if len(stopped) == 0 {
		fmt.Println("nothing to reap")
		return
	}
	for _, s := range stopped {
		fmt.Printf("stopped %s\n", s)
	}
}

// serviceJob builds a Job from CLI flags for a service, which has no
// submission directory.
func serviceJob(t *tool.Tool, name string, params, tags kvList) *job.Job {
	j := &job.Job{
		SchemaVersion: 1,
		Name:          name,
		Params:        map[string]any{},
		Tags:          map[string]string{},
	}
	if j.Name == "" {
		j.Name = t.Name
	}
	for _, kv := range params {
		j.Params[kv.key] = kv.value
	}
	for _, kv := range tags {
		j.Tags[kv.key] = kv.value
	}
	return j
}

func runJobList(args []string) {
	fs := newFlagSet("job list")
	configDir := configDirFlag(fs)
	_ = fs.String("user", "", "filter by user")
	_ = fs.String("tool", "", "filter by tool")
	var positional []string
	parseFlagsLoose(fs, args, &positional)

	insts, err := runtime.ListInstances(*configDir)
	if err != nil {
		fatalf("%v", err)
	}
	if len(insts) == 0 {
		fmt.Println("no instances recorded")
		return
	}
	// id 形如 <user>-<tool>-<slug>-<hex>，宽度按实际内容算，避免表格错位。
	wID, wTool, wState, wDur := len("INSTANCE"), len("TOOL"), len("STATE"), len("DURATION")
	for _, i := range insts {
		wID = max(wID, len(i.ID))
		wTool = max(wTool, len(i.Tool))
		wState = max(wState, len(i.State))
		wDur = max(wDur, len(i.Duration))
	}
	fmt.Printf("%-*s %-*s %-*s %-6s %-*s %s\n", wID, "INSTANCE", wTool, "TOOL", wState, "STATE", "EXIT", wDur, "DURATION", "NAME")
	for _, i := range insts {
		fmt.Printf("%-*s %-*s %-*s %-6d %-*s %s\n",
			wID, i.ID, wTool, i.Tool, wState, i.State, i.ExitCode, wDur, i.Duration, i.JobName)
	}
}

func runJobStatus(args []string) {
	fs := newFlagSet("job status")
	configDir := configDirFlag(fs)
	var positional []string
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: srcos job status <instance-id|job-id>") }
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		fs.Usage()
		os.Exit(1)
	}
	inst := mustFindInstance(*configDir, positional[0])
	printInstance(inst)
}

func runJobLogs(args []string) {
	fs := newFlagSet("job logs")
	configDir := configDirFlag(fs)
	var positional []string
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: srcos job logs <instance-id|job-id>") }
	parseFlagsLoose(fs, args, &positional)
	if len(positional) == 0 {
		fs.Usage()
		os.Exit(1)
	}
	inst := mustFindInstance(*configDir, positional[0])
	if inst.LogPath == "" {
		fatalf("instance %s has no log path", inst.ID)
	}
	data, err := os.ReadFile(inst.LogPath)
	if err != nil {
		fatalf("%v", err)
	}
	if len(data) == 0 {
		fmt.Printf("(log empty) %s\n", inst.LogPath)
		return
	}
	fmt.Printf("# %s\n", inst.LogPath)
	os.Stdout.Write(data)
	if data[len(data)-1] != '\n' {
		fmt.Println()
	}
}

func printInstance(i *runtime.Instance) {
	fmt.Printf("instance  %s\n", i.ID)
	fmt.Printf("  job       %s\n", i.JobName)
	fmt.Printf("  user/tool %s/%s  kind=%s\n", i.User, i.Tool, i.Kind)
	fmt.Printf("  state     %s (exit %d)\n", i.State, i.ExitCode)
	if i.Error != "" {
		fmt.Printf("  error     %s\n", i.Error)
	}
	fmt.Printf("  backend   %s  sandbox=%s  limiter=%s\n", i.Backend, i.Sandbox, i.Limiter)
	if i.Endpoint != "" {
		fmt.Printf("  endpoint  %s  route=%s\n", i.Endpoint, i.RoutePath)
	}
	if i.BackendRef != "" {
		fmt.Printf("  unit      %s\n", i.BackendRef)
	}
	fmt.Printf("  started   %s\n", i.StartedAt.Format(time.RFC3339))
	if !i.EndedAt.IsZero() {
		fmt.Printf("  ended     %s  (%s)\n", i.EndedAt.Format(time.RFC3339), i.Duration)
	}
	fmt.Printf("  workdir   %s\n", i.WorkDir)
	fmt.Printf("  log       %s\n", i.LogPath)
	if len(i.Mounts) > 0 {
		fmt.Printf("  mounts\n")
		for _, m := range i.Mounts {
			fmt.Printf("    %s\n", m)
		}
	}
	if len(i.Outputs) > 0 {
		fmt.Printf("  outputs\n")
		for _, o := range i.Outputs {
			fmt.Printf("    %s\n", o)
		}
	}
	if len(i.Tags) > 0 {
		keys := make([]string, 0, len(i.Tags))
		for k := range i.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("  tags      ")
		for _, k := range keys {
			fmt.Printf("%s=%s ", k, i.Tags[k])
		}
		fmt.Println()
	}
	fmt.Printf("  command   %s\n", strings.Join(i.Command, " "))
}

// ─────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func configDirFlag(fs *flag.FlagSet) *string {
	def := config.DefaultConfigDir()
	dir := fs.String("config-dir", def, "config directory (default <program dir>/config)")
	fs.StringVar(dir, "d", def, "shorthand for --config-dir")
	return dir
}

// parseFlagsLoose allows flags and positionals to interleave.
//
// Go's flag package stops at the first non-flag argument, so a command like
// `srcos grant group bio --user alice` would silently ignore --user and then
// complain that the group is empty. Re-parsing after each positional consumes
// them in any order, which is what a person typing a command expects.
func parseFlagsLoose(fs *flag.FlagSet, args []string, positional *[]string) {
	for {
		if err := fs.Parse(args); err != nil {
			os.Exit(1)
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return
		}
		*positional = append(*positional, rest[0])
		args = rest[1:]
	}
}

// kvList collects repeated key=value flags.
type kvList []kv

type kv struct{ key, value string }

func (l *kvList) String() string {
	parts := make([]string, 0, len(*l))
	for _, item := range *l {
		parts = append(parts, item.key+"="+item.value)
	}
	return strings.Join(parts, ",")
}

func (l *kvList) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("want key=value, got %q", s)
	}
	*l = append(*l, kv{key: strings.TrimSpace(k), value: v})
	return nil
}

// strList collects repeated plain-string flags (e.g. --output /path).
type strList []string

func (l *strList) String() string { return strings.Join(*l, ",") }

func (l *strList) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("empty value")
	}
	*l = append(*l, s)
	return nil
}

// resolveToolsDir mirrors config.ResolveToolsDir, with the CLI override on top.
func resolveToolsDir(override, configDir string) string {
	if override != "" {
		return override
	}
	return config.ResolveToolsDir(configDir)
}

func mustLoadTool(toolsDir, configDir, id string) (*tool.Tool, string) {
	if strings.TrimSpace(id) == "" {
		fatalf("--tool is required")
	}
	root := resolveToolsDir(toolsDir, configDir)
	t, err := tool.Find(root, id)
	if err != nil {
		fatalf("%v\n  (hint: --tools-dir points at the root containing <tool-id>/tool.yaml; currently %s)", err, root)
	}
	return t, root
}

func resolveUser(flagValue string) string {
	u := flagValue
	if u == "" {
		u = os.Getenv("SRCOS_USER")
	}
	if u == "" {
		u = os.Getenv("USER")
	}
	if u == "" {
		u = os.Getenv("LOGNAME")
	}
	if u == "" {
		fatalf("cannot determine user: pass --user or set $USER")
	}
	return u
}

func jobIDMatches(id, needle string) bool {
	return id == needle ||
		strings.HasPrefix(id, needle+"-") ||
		strings.HasSuffix(id, "-"+needle)
}

func mustFindInstance(configDir, needle string) *runtime.Instance {
	insts, err := runtime.ListInstances(configDir)
	if err != nil {
		fatalf("%v", err)
	}
	// Accept either the full instance id or just the job id.
	for _, i := range insts {
		if i.ID == needle {
			return i
		}
	}
	for _, i := range insts {
		if strings.HasSuffix(i.ID, "-"+needle) {
			return i
		}
	}
	fatalf("no instance matching %q under %s", needle, config.InstancesDir(configDir))
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fatalf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	os.Exit(1)
}
