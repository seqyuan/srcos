package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTool(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const minimalTask = `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: x, type: string, required: true}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 2, memory: "2Gi", walltime: "0:10:00"}
`

const minimalWork = "#!/bin/sh\nexit 0\n"

func TestLoadValidTask(t *testing.T) {
	dir := writeTool(t, map[string]string{"tool.yaml": minimalTask, "work.sh": minimalWork})
	tl, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tl.ID != "demo" || tl.Kind != KindTask || tl.Backend != BackendLocal {
		t.Fatalf("unexpected tool: %+v", tl)
	}
	if tl.Sandbox != SandboxNone {
		t.Fatalf("sandbox should default to none, got %q", tl.Sandbox)
	}
	if tl.Dir != dir {
		t.Fatalf("Dir not recorded: %q", tl.Dir)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "task without walltime",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
`,
			wantSub: "requires resources.walltime",
		},
		{
			name: "task with ingress",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
ingress: {port: 8080}
`,
			wantSub: "must not declare ingress",
		},
		{
			name: "service without lifecycle",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: service
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080}
`,
			wantSub: "kind: service requires lifecycle",
		},
		{
			name: "apptainer without image",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: apptainer
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "requires image",
		},
		{
			// The ADR-007 cross-check: most SGE sites forbid nested qsub, so a
			// submitter must run on the login node.
			name: "qsubsge executor on sge backend",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: sge
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
internal: {executor: qsubsge}
`,
			wantSub: "illegal",
		},
		{
			name: "path input without from",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: ref, type: path}
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "type path requires from",
		},
		{
			name: "path input from undeclared storage",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: ref, type: path, from: cluster-share}
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "not listed in requires_storages",
		},
		{
			name: "storages without a mount namespace",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: none
entry: work.sh
requires_storages: [data]
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "sandbox: none with requires_storages",
		},
		{
			name: "enum without values",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: mode, type: enum}
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "requires values",
		},
		{
			name: "duplicate interface name across inputs and outputs",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: outs, type: string}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "duplicate name",
		},
		{
			name: "output type must be file or directory",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  outputs:
    - {name: outs, type: string}
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "type must be file|directory",
		},
		{
			name: "bad id",
			yaml: `
schemaVersion: 1
id: "Bad_ID"
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "must match",
		},
		{
			name: "missing entry file",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: nope.sh
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`,
			wantSub: "not found in tool directory",
		},
		{
			name: "bad memory",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "32 gigs", walltime: "0:01:00"}
`,
			wantSub: "invalid memory",
		},
		{
			name: "bad walltime",
			yaml: `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi", walltime: "4h"}
`,
			wantSub: "invalid walltime",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeTool(t, map[string]string{"tool.yaml": tc.yaml, "work.sh": minimalWork})
			_, err := Load(dir)
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateCollectsAllProblems(t *testing.T) {
	// A manifest with several independent mistakes should report all of them,
	// because a tool author wants the whole list in one pass.
	dir := writeTool(t, map[string]string{"tool.yaml": `
schemaVersion: 1
id: "BAD ID"
version: 0.1.0
name: Demo
kind: task
backend: k8s
sandbox: apptainer
entry: work.sh
interface:
  outputs:
    - {name: outs, type: string}
resources: {cpu: 0, memory: "nope"}
`, "work.sh": minimalWork})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"must match", "backend", "requires image", "type must be file|directory", "invalid memory", "must be >= 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing problem %q in:\n%s", want, msg)
		}
	}
}

func TestDiscoverRejectsDuplicateID(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(minimalTask), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte(minimalWork), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("expected duplicate id error")
	} else if !strings.Contains(err.Error(), "duplicate tool id") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiscoverSkipsNonToolDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "not-a-tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestParseMemory(t *testing.T) {
	cases := map[string]uint64{
		"1024": 1024,
		"1Ki":  1 << 10,
		"1Mi":  1 << 20,
		"32Gi": 32 << 30,
		"512M": 512e6,
		"2G":   2e9,
		"1Ti":  1 << 40,
		"0":    0,
	}
	for in, want := range cases {
		got, err := ParseMemory(in)
		if err != nil {
			t.Errorf("ParseMemory(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMemory(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "nope", "32 gigs", "-1Gi", "1.2.3Gi"} {
		if _, err := ParseMemory(bad); err == nil {
			t.Errorf("ParseMemory(%q) should fail", bad)
		}
	}
}

func TestFormatMemory(t *testing.T) {
	cases := map[uint64]string{
		32 << 30:  "32G",
		512 << 20: "512M",
		0:         "0",
		1536:      "1536", // not a whole K
	}
	for in, want := range cases {
		if got := FormatMemory(in); got != want {
			t.Errorf("FormatMemory(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseWalltime(t *testing.T) {
	cases := map[string]int64{
		"":        0,
		"0:10:00": 600,
		"4:00:00": 4 * 3600,
		"90:00":   90 * 60,
		"1:02:03": 3723,
	}
	for in, want := range cases {
		got, err := ParseWalltime(in)
		if err != nil {
			t.Errorf("ParseWalltime(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseWalltime(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"4h", "1:2:3:4", "abc", "-1:00"} {
		if _, err := ParseWalltime(bad); err == nil {
			t.Errorf("ParseWalltime(%q) should fail", bad)
		}
	}
}

// TestLoadRealHelloFanout pins the shipped example to the contract. If the
// spec and the example drift apart, this fails.
func TestLoadRealHelloFanout(t *testing.T) {
	dir := filepath.Join("..", "..", "srcos-tools", "hello-fanout")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("example tool not present: %v", err)
	}
	tl, err := Load(dir)
	if err != nil {
		t.Fatalf("shipped hello-fanout must validate: %v", err)
	}
	if tl.Kind != KindTask || tl.Sandbox != SandboxBwrap || tl.Backend != BackendLocal {
		t.Fatalf("unexpected example config: kind=%s sandbox=%s backend=%s", tl.Kind, tl.Sandbox, tl.Backend)
	}
	if tl.Resources.CPU != 5 {
		t.Fatalf("example cpu should be the aggregate 5, got %d", tl.Resources.CPU)
	}
	var samples *Input
	for i := range tl.Interface.Inputs {
		if tl.Interface.Inputs[i].Name == "samples" {
			samples = &tl.Interface.Inputs[i]
		}
	}
	if samples == nil || !samples.Required {
		t.Fatal("example must declare a required `samples` input")
	}
}

func TestIngressBackendPathMustBeAbsolute(t *testing.T) {
	// relative backend_path is refused at registration: it would be a second,
	// accidental contract about where the app lives.
	if _, err := Load(writeTool(t, map[string]string{"tool.yaml": `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080, backend_path: app}
lifecycle: {restart: never, max_lifetime: "1h"}
`, "work.sh": minimalWork})); err == nil {
		t.Fatal("a relative ingress.backend_path must be rejected")
	}
}

func TestEnvReservedKeysRejected(t *testing.T) {
	base := func(env string) string {
		return `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: bwrap
entry: work.sh
` + env + `
resources: {cpu: 1, memory: "1Gi", walltime: "0:01:00"}
`
	}
	// The platform's own variables may not be repointed.
	if _, err := Load(writeTool(t, map[string]string{
		"tool.yaml": base("env: [\"HOME=/tmp\"]"), "work.sh": minimalWork})); err == nil {
		t.Fatal("overriding HOME must be rejected")
	}
	if _, err := Load(writeTool(t, map[string]string{
		"tool.yaml": base("env: [\"SRCOS_WORKSPACE=/etc\"]"), "work.sh": minimalWork})); err == nil {
		t.Fatal("overriding a SRCOS_* variable must be rejected")
	}
	// A malformed entry is caught too, and a normal one passes.
	if _, err := Load(writeTool(t, map[string]string{
		"tool.yaml": base("env: [\"NOVALUE\"]"), "work.sh": minimalWork})); err == nil {
		t.Fatal("an entry without '=' must be rejected")
	}
	if _, err := Load(writeTool(t, map[string]string{
		"tool.yaml": base("env: [\"PATH=/opt/x/bin:/usr/bin\"]"), "work.sh": minimalWork})); err != nil {
		t.Fatalf("a plain env entry must be accepted: %v", err)
	}
}

// 声明式 command：与 entry 二选一，且 ${...} 只能引用平台给这个 kind 的变量。
func TestCommandValidation(t *testing.T) {
	base := func(extra string) string {
		return `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: service
backend: local
sandbox: bwrap
` + extra + `
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080}
lifecycle: {restart: never, max_lifetime: "1h"}
`
	}
	load := func(extra string) error {
		_, err := Load(writeTool(t, map[string]string{"tool.yaml": base(extra)}))
		return err
	}

	for _, tc := range []struct{ name, extra, want string }{
		{"both command and entry", "entry: work.sh\ncommand: [\"true\"]\n", "mutually exclusive"},
		{"neither", "", "one of entry or command is required"},
		{"empty program", "command: [\"  \"]\n", "command[0]"},
		{"unknown variable", "command: [\"run\", \"${SRCOS_NOPE}\"]\n", "not a variable SRCOS provides"},
		{"task-only variable in a service", "command: [\"run\", \"${SRCOS_TASK_ID}\"]\n", "not a variable SRCOS provides"},
		{"unterminated reference", "command: [\"run\", \"${SRCOS_PORT\"]\n", "unterminated"},
	} {
		err := load(tc.extra)
		if err == nil {
			t.Fatalf("%s: expected a registration error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	// A service may reference its port and workspace; the program is the first
	// element.
	if err := load("command: [\"serve\", \"--port\", \"${SRCOS_PORT}\", \"${SRCOS_WORKSPACE}\"]\n"); err != nil {
		t.Fatalf("a valid command must be accepted: %v", err)
	}
}

func TestExpandCommand(t *testing.T) {
	env := []string{"SRCOS_PORT=20000", "SRCOS_WORKSPACE=/workspace", "PATH=/usr/bin"}

	got, err := ExpandCommand(
		[]string{"serve", "--port", "${SRCOS_PORT}", "--dir=${SRCOS_WORKSPACE}", "$SRCOS_PORT", "100%"},
		env)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"serve", "--port", "20000", "--dir=/workspace", "$SRCOS_PORT", "100%"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ExpandCommand = %q, want %q", got, want)
	}

	// A reference the environment does not carry is an error, never an empty
	// string (an empty port reads as "started but unreachable").
	if _, err := ExpandCommand([]string{"serve", "${SRCOS_MISSING}"}, env); err == nil {
		t.Fatal("a missing variable must be an error")
	}
}

// ingress 的可选项：port 只是偏好（local 用 $SRCOS_PORT），websocket 默认开，
// bwlimit 不得为负。
func TestIngressOptionalFields(t *testing.T) {
	base := func(ingress string) string {
		return `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
` + ingress + `
lifecycle: {restart: never, max_lifetime: "1h"}
`
	}
	load := func(ingress string) (*Tool, error) {
		return Load(writeTool(t, map[string]string{"tool.yaml": base(ingress), "work.sh": minimalWork}))
	}

	// No port at all: valid (local assigns SRCOS_PORT).
	tl, err := load("ingress: {healthcheck: {path: /}}")
	if err != nil {
		t.Fatalf("ingress without a port must be valid: %v", err)
	}
	if !tl.Ingress.WebSocketEnabled() {
		t.Fatal("websocket must default to enabled")
	}
	if tl.Ingress.BWLimit != 0 {
		t.Fatalf("bwlimit = %d, want 0 (unlimited)", tl.Ingress.BWLimit)
	}

	if _, err := load("ingress: {port: -1}"); err == nil || !strings.Contains(err.Error(), "ingress.port") {
		t.Fatalf("a negative port must be rejected: %v", err)
	}
	if _, err := load("ingress: {port: 8080, bwlimit: -5}"); err == nil || !strings.Contains(err.Error(), "bwlimit") {
		t.Fatalf("a negative bwlimit must be rejected: %v", err)
	}
	if _, err := load("ingress: {port: 8080, websocket: false}"); err != nil {
		t.Fatalf("an explicit websocket: false must be accepted: %v", err)
	}
}

// backend: external —— 转发一个已经跑着的后端。SRCOS 不启动它，所以描述
// 「怎么跑」的字段全是死配置，逐个拒绝。
func TestExternalBackendValidation(t *testing.T) {
	base := func(extra string) string {
		return `
schemaVersion: 1
id: shiny-server
version: 0.1.0
name: Shiny Server
kind: service
backend: external
` + extra + `
ingress: {healthcheck: {path: /}}
`
	}
	load := func(extra string) (*Tool, error) {
		return Load(writeTool(t, map[string]string{"tool.yaml": base(extra)}))
	}

	tl, err := load("external: {host: 127.0.0.1, port: 3838}")
	if err != nil {
		t.Fatalf("a forwarded backend needs no command/entry/lifecycle/resources: %v", err)
	}
	if tl.External.Endpoint() != "127.0.0.1:3838" {
		t.Fatalf("endpoint = %q", tl.External.Endpoint())
	}

	for _, tc := range []struct{ name, extra, want string }{
		{"no external block", "", "requires an `external:"},
		{"remote host", "external: {host: 10.0.0.5, port: 80}", "must be loopback"},
		{"port out of range", "external: {host: 127.0.0.1, port: 0}", "out of range"},
		{"entry is dead config", "external: {host: 127.0.0.1, port: 3838}\nentry: work.sh", "would never run"},
		{"environment is dead config", "external: {host: 127.0.0.1, port: 3838}\nenvironment: r-miniforge", "would never be used"},
		{"storages are dead config", "external: {host: 127.0.0.1, port: 3838}\nrequires_storages: [data]", "would never be mounted"},
		{"agent cannot be hosted", "external: {host: 127.0.0.1, port: 3838}\nagent: {mcp: [read]}", "cannot host an agent"},
	} {
		if _, err := load(tc.extra); err == nil {
			t.Fatalf("%s: expected an error", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	// external is only meaningful with backend: external.
	if _, err := Load(writeTool(t, map[string]string{"tool.yaml": `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
entry: work.sh
external: {host: 127.0.0.1, port: 3838}
ingress: {healthcheck: {path: /}}
lifecycle: {max_lifetime: "1h"}
`, "work.sh": minimalWork})); err == nil || !strings.Contains(err.Error(), "only meaningful with backend: external") {
		t.Fatalf("external with a local backend must be rejected: %v", err)
	}
}
