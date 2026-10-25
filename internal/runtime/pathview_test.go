package runtime

import (
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/tool"
)

func envHas(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

// SRCOS_API is the channel a tool UI — or an agent hosted by a service — uses to
// call SRCOS back (roadmap §4.4). It is injected only when a gateway is reachable
// from here, so a tool can tell "no gateway" from "call failed".
func TestPathViewEnvIncludesSRCOSAPI(t *testing.T) {
	p := PathsFor("/cfg", "alice", "demo", "")
	tl := &tool.Tool{ID: "demo", Version: "0.1.0"}

	with := NewPathView(tool.SandboxBwrap, p, "http://127.0.0.1:30152/api", nil).Env(tl, nil)
	if !envHas(with, "SRCOS_API=http://127.0.0.1:30152/api") {
		t.Fatalf("SRCOS_API missing from %v", with)
	}

	without := NewPathView(tool.SandboxBwrap, p, "", nil).Env(tl, nil)
	for _, kv := range without {
		if strings.HasPrefix(kv, "SRCOS_API=") {
			t.Fatalf("SRCOS_API must be absent when no gateway is known: %v", without)
		}
	}
}

// The tool's declared environment is appended last, so it wins over the
// platform defaults for the same key (a tool that sets PATH means it).
func TestPathViewEnvAppendsToolEnvLast(t *testing.T) {
	tl := &tool.Tool{ID: "demo", Version: "1", Env: []string{"PATH=/opt/x/bin:/usr/bin", "LANG=C.UTF-8"}}
	env := NewPathView(tool.SandboxBwrap, PathsFor("/cfg", "alice", "demo", ""), "", nil).Env(tl, nil)

	if !envHas(env, "PATH=/opt/x/bin:/usr/bin") || !envHas(env, "LANG=C.UTF-8") {
		t.Fatalf("the tool's env did not reach the unit: %v", env)
	}
	// Both of its entries must come after the platform's own variables, because
	// a later duplicate wins.
	first := -1
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=/opt/x/bin") {
			first = i
			break
		}
	}
	if first != len(env)-2 { // PATH then LANG, in declaration order
		t.Fatalf("tool env should be appended last, got index %d of %d: %v", first, len(env), env)
	}
}

// A component manifest lives in ui/ or task/ but its entry belongs to the
// shared package root. The view must expose the package root, not the
// manifest's directory, or /tool and SRCOS_TOOL_DIR disagree with the mount.
func TestPathViewUsesPackageRoot(t *testing.T) {
	tl := &tool.Tool{
		ID:          "scrna-task",
		Dir:         "/host/pkg/task",
		PackageRoot: "/host/pkg",
		Entry:       "work.sh",
		EntryRel:    "task/work.sh",
	}
	p := PathsFor("/cfg", "alice", "scrna-task", "")

	degraded := NewPathView(tool.SandboxNone, p, "", nil)
	if got := degraded.ToolDir(tl); got != "/host/pkg" {
		t.Fatalf("degraded ToolDir = %q, want the package root", got)
	}
	if env := degraded.Env(tl, nil); !envHas(env, "SRCOS_TOOL_DIR=/host/pkg") {
		t.Fatalf("SRCOS_TOOL_DIR missing from degraded env: %v", env)
	}

	sandboxed := NewPathView(tool.SandboxBwrap, p, "", nil)
	if got := sandboxed.ToolDir(tl); got != "/tool" {
		t.Fatalf("sandbox ToolDir = %q, want /tool", got)
	}
	if env := sandboxed.Env(tl, nil); !envHas(env, "SRCOS_TOOL_DIR=/tool") {
		t.Fatalf("SRCOS_TOOL_DIR missing from sandbox env: %v", env)
	}
}

func TestResolveArgvUsesPackageRelativeEntry(t *testing.T) {
	tl := &tool.Tool{ID: "scrna-task", Entry: "work.sh", EntryRel: "task/work.sh"}
	view := NewPathView(tool.SandboxBwrap, PathsFor("/cfg", "alice", "scrna-task", ""), "", nil)
	argv, err := ResolveArgv(tl, view, "", nil)
	if err != nil {
		t.Fatalf("ResolveArgv: %v", err)
	}
	if len(argv) != 2 || argv[1] != "/tool/task/work.sh" {
		t.Fatalf("argv = %v, want [bash /tool/task/work.sh]", argv)
	}
}

// /tool must be the shared package root, not the component manifest directory;
// otherwise UI and task components cannot reach sibling shared/ scripts.
func TestBuildSpecMountsPackageRoot(t *testing.T) {
	pkg := t.TempDir()
	tl := &tool.Tool{ID: "scrna-task", Dir: pkg + "/task", PackageRoot: pkg}
	spec, err := BuildSpec(tl, PathsFor("/cfg", "alice", "scrna-task", "job-1"), nil)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	host, mount, err := spec.Resolve("/tool/task/work.sh")
	if err != nil {
		t.Fatalf("Resolve /tool: %v", err)
	}
	if mount.HostPath != pkg || host != pkg+"/task/work.sh" {
		t.Fatalf("/tool resolved to mount %q host %q, want %q", mount.HostPath, host, pkg)
	}
}

// A network-isolated task has its own netns: 127.0.0.1 is its own loopback,
// not the gateway's. Do not advertise an SRCOS_API URL that cannot work; a
// task that needs the callback declares network: host.
func TestPathViewOmitsSRCOSAPIForNetworkIsolatedTasks(t *testing.T) {
	tl := &tool.Tool{ID: "demo", Kind: tool.KindTask, Network: tool.NetworkNone}
	env := NewPathView(tool.SandboxBwrap, PathsFor("/cfg", "alice", "demo", "j1"),
		"http://127.0.0.1:30152/api", nil).Env(tl, nil)
	for _, kv := range env {
		if strings.HasPrefix(kv, "SRCOS_API=") {
			t.Fatalf("SRCOS_API was injected into an isolated task: %v", env)
		}
	}
}
