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

	with := NewPathView(tool.SandboxBwrap, p, "http://127.0.0.1:30152/api").Env(tl, nil)
	if !envHas(with, "SRCOS_API=http://127.0.0.1:30152/api") {
		t.Fatalf("SRCOS_API missing from %v", with)
	}

	without := NewPathView(tool.SandboxBwrap, p, "").Env(tl, nil)
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
	env := NewPathView(tool.SandboxBwrap, PathsFor("/cfg", "alice", "demo", ""), "").Env(tl, nil)

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
