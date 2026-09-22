package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Each tool is exercised end to end through the protocol (not by calling the
// handler directly): what an agent gets is the JSON-RPC answer, so that is what
// these tests assert on.

func TestToolListTools(t *testing.T) {
	h := newHarness(t)
	out := h.callTool(t, "srcos_list_tools", `{}`).(map[string]any)

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", out)
	}
	entry, _ := tools[0].(map[string]any)
	if entry["id"] != "demo" || entry["kind"] != "task" || entry["backend"] != "local" {
		t.Fatalf("entry = %v", entry)
	}
	// The parameter names are what tells an agent which inputs to describe.
	inputs, _ := entry["inputs"].([]any)
	if len(inputs) != 2 || inputs[0] != "ref" || inputs[1] != "samples" {
		t.Fatalf("inputs = %v", inputs)
	}
}

func TestToolDescribeTool(t *testing.T) {
	h := newHarness(t)
	out := h.callTool(t, "srcos_describe_tool", `{"tool":"demo"}`).(map[string]any)

	if out["kind"] != "task" || out["entry"] != "work.sh" {
		t.Fatalf("describe = %v", out)
	}
	// The derived JSON Schema is the point of ADR-018's first derivation.
	schema, _ := out["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("inputSchema properties = %v", props)
	}
	ref, _ := props["ref"].(map[string]any)
	// The path parameter's description has to say where values come from: an
	// agent cannot discover a storage selection on its own.
	desc, _ := ref["description"].(string)
	if ref["type"] != "string" || !strings.Contains(desc, "srcos_list_paths") || !strings.Contains(desc, "data") {
		t.Fatalf("path parameter schema = %v", ref)
	}
	required, _ := schema["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("required = %v", required)
	}
	// The raw interface travels with the schema, so a UI can render a form from
	// the same answer.
	if _, ok := out["interface"].(map[string]any); !ok {
		t.Fatalf("interface = %v", out["interface"])
	}
	storages, _ := out["storages"].([]any)
	if len(storages) != 1 {
		t.Fatalf("storages = %v", storages)
	}
	s, _ := storages[0].(map[string]any)
	if s["id"] != "data" || s["root"] != "/data" {
		t.Fatalf("storage view = %v", s)
	}

	// An unknown tool is a tool *execution* error (the question was understood
	// and refused), not a protocol error.
	msg := h.callToolError(t, "srcos_describe_tool", `{"tool":"nope"}`)
	if !strings.Contains(msg, "not found") {
		t.Fatalf("message = %q", msg)
	}
	// A missing argument is refused too.
	if msg := h.callToolError(t, "srcos_describe_tool", `{}`); !strings.Contains(msg, "tool is required") {
		t.Fatalf("message = %q", msg)
	}
}

func TestToolListStorages(t *testing.T) {
	h := newHarness(t)
	out := h.callTool(t, "srcos_list_storages", `{}`).(map[string]any)

	storages, _ := out["storages"].([]any)
	if len(storages) != 1 {
		t.Fatalf("storages = %v", out)
	}
	s, _ := storages[0].(map[string]any)
	if s["id"] != "data" || s["mode"] != "ro" {
		t.Fatalf("storage = %v", s)
	}
	tools, _ := s["tools"].([]any)
	if len(tools) != 1 || tools[0] != "demo" {
		t.Fatalf("tools = %v", tools)
	}
	// The host root never appears: it is SRCOS's business, not a caller's.
	if strings.Contains(h.callTool(t, "srcos_list_storages", `{}`).(map[string]any)["storages"].([]any)[0].(map[string]any)["root"].(string), h.dataRoot) {
		t.Fatal("a host path leaked into the answer")
	}
}

func TestToolListPaths(t *testing.T) {
	h := newHarness(t)
	out := h.callTool(t, "srcos_list_paths", `{"tool":"demo","input":"ref"}`).(map[string]any)

	if out["storage"] != "data" || out["path"] != "/data" {
		t.Fatalf("listing = %v", out)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	entry, _ := entries[0].(map[string]any)
	if entry["path"] != "/data/ref" || entry["isDir"] != true {
		t.Fatalf("entry = %v", entry)
	}

	// Descent gives a parent to go back up to.
	out = h.callTool(t, "srcos_list_paths", `{"tool":"demo","input":"ref","path":"/data/ref"}`).(map[string]any)
	if out["parent"] != "/data" {
		t.Fatalf("parent = %v", out["parent"])
	}

	// The closure: a storage the tool does not declare is refused, whatever the
	// caller asks for.
	msg := h.callToolError(t, "srcos_list_paths", `{"tool":"demo","input":"ref","storage":"elsewhere"}`)
	if !strings.Contains(msg, "not declared") {
		t.Fatalf("message = %q", msg)
	}
	// And an escape is refused by the jail.
	if msg := h.callToolError(t, "srcos_list_paths", `{"tool":"demo","input":"ref","path":"/data/../.."}`); msg == "" {
		t.Fatal("an escaping path must be refused")
	}
	// A parameter that is not storage-bound has nothing to list.
	if msg := h.callToolError(t, "srcos_list_paths", `{"tool":"demo","input":"samples"}`); !strings.Contains(msg, "not storage-bound") {
		t.Fatalf("message = %q", msg)
	}
}

func TestToolInstancesStatusAndLogs(t *testing.T) {
	h := newHarness(t)

	out := h.callTool(t, "srcos_list_instances", `{}`).(map[string]any)
	instances, _ := out["instances"].([]any)
	if len(instances) != 1 {
		t.Fatalf("instances = %v", out)
	}
	inst, _ := instances[0].(map[string]any)
	if inst["id"] != "alice-demo-run-1" || inst["state"] != "succeeded" {
		t.Fatalf("instance = %v", inst)
	}
	tags, _ := inst["tags"].(map[string]any)
	if tags["project"] != "p1" {
		t.Fatalf("tags = %v", tags)
	}

	// A suffix is enough, and the status answer carries the artifacts summary.
	out = h.callTool(t, "srcos_task_status", `{"instance":"run-1"}`).(map[string]any)
	status, _ := out["instance"].(map[string]any)
	if status["id"] != "alice-demo-run-1" {
		t.Fatalf("status = %v", out)
	}
	arts, _ := out["artifacts"].([]any)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %v", arts)
	}

	out = h.callTool(t, "srcos_task_logs", `{"instance":"run-1"}`).(map[string]any)
	if log, _ := out["log"].(string); !strings.Contains(log, "line two") {
		t.Fatalf("log = %v", out)
	}

	// Unknown instance: a clear execution error.
	if msg := h.callToolError(t, "srcos_task_status", `{"instance":"nope"}`); !strings.Contains(msg, "no instance") {
		t.Fatalf("message = %q", msg)
	}
}

func TestToolListArtifacts(t *testing.T) {
	h := newHarness(t)
	out := h.callTool(t, "srcos_list_artifacts", `{"instance":"run-1"}`).(map[string]any)

	arts, _ := out["artifacts"].([]any)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %v", out)
	}
	present, _ := arts[0].(map[string]any)
	if present["path"] != "/workspace/out" || present["exists"] != true || present["entries"].(float64) != 1 {
		t.Fatalf("present artifact = %v", present)
	}
	absent, _ := arts[1].(map[string]any)
	if absent["exists"] != false || absent["note"] != "missing" {
		t.Fatalf("missing artifact = %v", absent)
	}
}

func TestToolReadFile(t *testing.T) {
	h := newHarness(t)

	out := h.callTool(t, "srcos_read_file", `{"path":"/data/ref/genes.txt"}`).(map[string]any)
	if out["text"] != "A\nB\n" || out["path"] != "/data/ref/genes.txt" {
		t.Fatalf("read = %v", out)
	}

	// A workspace path needs the tool that owns it.
	out = h.callTool(t, "srcos_read_file", `{"path":"/workspace/out/report.tsv","tool":"demo"}`).(map[string]any)
	if out["text"] != "x\n" {
		t.Fatalf("workspace read = %v", out)
	}
	if msg := h.callToolError(t, "srcos_read_file", `{"path":"/workspace/out/report.tsv"}`); !strings.Contains(msg, "not permitted") {
		t.Fatalf("message = %q", msg)
	}

	// Everything else is refused: the range is narrow on purpose.
	for _, path := range []string{"/etc/passwd", "/tool/work.sh", "/data/../../etc/passwd", "/home/nobody/notes"} {
		h.callToolError(t, "srcos_read_file", `{"path":"`+path+`"}`)
	}
}

// The answer must be copy-pasteable JSON: an agent that needs a path or an id
// reads it exactly, so the rendering is part of the contract.
func TestRenderIsStableJSON(t *testing.T) {
	out := render(map[string]any{"b": 1, "a": []string{"x"}})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("render is not JSON: %v\n%s", err, out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatal("render should be indented for a model to read")
	}
	if render("plain") != "plain" {
		t.Fatal("a string answer must pass through unchanged")
	}
}

func TestArgumentHelpersRejectTheWrongType(t *testing.T) {
	args := map[string]any{"ok": "value", "num": float64(7), "numStr": "8", "bad": 3, "nil": nil}
	if optString(args, "ok") != "value" || optString(args, "nil") != "" {
		t.Fatal("optString")
	}
	// A wrong type is not silently coerced: `{"tool": 3}` must not become "3".
	if optString(args, "bad") != "" {
		t.Fatal("a non-string must not be stringified")
	}
	if optInt(args, "num") != 7 || optInt(args, "numStr") != 8 || optInt(args, "ok") != 0 {
		t.Fatal("optInt")
	}
	if _, err := requireString(args, "missing"); err == nil {
		t.Fatal("requireString must fail on a missing argument")
	}
	if v, err := requireString(args, "ok"); err != nil || v != "value" {
		t.Fatal("requireString")
	}
}
