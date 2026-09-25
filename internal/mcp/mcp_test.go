package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
)

// harness is a whole little deployment: one user, one task tool with a declared
// storage, one storage root, one finished instance, and an agent token.
type harness struct {
	*Server
	handler     http.Handler
	token       string
	submitToken string
	configDir   string
	toolsDir    string
	dataRoot    string
	registry    *config.UserRegistry
}

const demoManifest = `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
description: "demo tool"
kind: task
backend: local
sandbox: bwrap
entry: work.sh
interface:
  inputs:
    - {name: ref, type: path, from: data, select: directory, required: true, description: "reference set"}
    - {name: samples, type: string, required: true}
  outputs:
    - {name: out, type: directory}
resources: {cpu: 1, memory: "1Gi", walltime: "0:10:00"}
requires_storages: [data]
`

func newHarness(t *testing.T) *harness {
	t.Helper()
	configDir := t.TempDir()
	userPath := config.UserConfigPath(configDir, "alice")
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("auth:\n  password_hash: \""+strings.Repeat("1", 64)+"\"\nservices: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	toolsDir := t.TempDir()
	toolDir := filepath.Join(toolsDir, "demo")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"), []byte(demoManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	dataRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataRoot, "ref"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "ref", "genes.txt"), []byte("A\nB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, err := storage.New([]storage.Storage{{
		ID: "data", Name: "Data", Kind: storage.KindPosix,
		HostRoot: dataRoot, SandboxPath: "/data", Mode: storage.ReadOnly,
	}})
	if err != nil {
		t.Fatal(err)
	}

	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "demo", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	registry := config.NewUserRegistry(configDir)
	registry.Reload()

	// One finished task instance with a real output on disk.
	paths := runtime.PathsFor(configDir, "alice", "demo", "run-1")
	if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(paths.Workspace, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Workspace, "out", "report.tsv"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LogPath, []byte("line one\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID:        runtime.InstanceID("alice", "demo", "run-1"),
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   "run-1",
		State:     runtime.StateSucceeded,
		Backend:   "local",
		Sandbox:   "none",
		LogPath:   paths.LogPath,
		WorkDir:   paths.JobDir,
		Outputs:   []string{"/workspace/out", "/workspace/nothing"},
		Tags:      map[string]string{"project": "p1"},
		StartedAt: time.Now().UTC(),
	}
	if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
		t.Fatal(err)
	}

	reader := &inspect.Reader{ConfigDir: configDir, ToolsDir: toolsDir, Storages: provider, Grants: policy}

	store := agenttoken.New(config.AgentTokensPath(configDir))
	store.AttachUserCheck(registry)
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	_, token, err := store.Create(agenttoken.CreateParams{User: "alice", Label: "test", Scopes: []agenttoken.Scope{agenttoken.ScopeRead}})
	if err != nil {
		t.Fatal(err)
	}
	// A second credential with the submit scope, so the write half can be
	// exercised without weakening the read one.
	_, submitToken, err := store.Create(agenttoken.CreateParams{
		User: "alice", Label: "ci", Scopes: []agenttoken.Scope{agenttoken.ScopeSubmit}, SubmitTools: []string{"demo"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The write path is wired with a runner that refuses to start anything: the
	// MCP layer's job is to decide *whether* to act, and a unit test must not
	// spawn processes.
	ctrl := execute.New(execute.Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		Storages:  provider,
		Grants:    policy,
		NewRunner: func(user string) (*runtime.Runner, error) {
			return nil, errNoRunner
		},
	})

	srv := NewServer("test-version", reader, store, ctrl)
	return &harness{
		Server:      srv,
		handler:     srv,
		token:       token,
		submitToken: submitToken,
		configDir:   configDir,
		toolsDir:    toolsDir,
		dataRoot:    dataRoot,
		registry:    registry,
	}
}

// errNoRunner stands in for a deployment where starting a unit is not possible
// in a unit test.
var errNoRunner = fmt.Errorf("no runner in this test")

// useToken runs fn with a different credential, then restores the harness's.
func (h *harness) useToken(tok string, fn func()) {
	old := h.token
	h.token = tok
	defer func() { h.token = old }()
	fn()
}

// call posts one JSON-RPC message and returns the decoded response.
func (h *harness) call(t *testing.T, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", Endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	var decoded map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, rec.Body)
		}
	}
	return rec, decoded
}

// rpc calls a method with params and fails when the response carries an error.
func (h *harness) rpc(t *testing.T, id, method, params string) map[string]any {
	t.Helper()
	rec, body := h.call(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"`+method+`","params":`+params+`}`)
	if rec.Code != 200 {
		t.Fatalf("%s: HTTP %d %s", method, rec.Code, rec.Body)
	}
	if err, ok := body["error"]; ok {
		t.Fatalf("%s: %v", method, err)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no result object: %s", method, rec.Body)
	}
	return result
}

// callTool invokes a tool and returns its text content, failing on isError.
func (h *harness) callTool(t *testing.T, name string, args string) any {
	t.Helper()
	result := h.rpc(t, "1", "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("%s: tool error: %v", name, result["content"])
	}
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("%s: content = %v", name, result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return text
	}
	return decoded
}

// callToolError invokes a tool expecting an isError result.
func (h *harness) callToolError(t *testing.T, name string, args string) string {
	t.Helper()
	result := h.rpc(t, "1", "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
	if isErr, _ := result["isError"].(bool); !isErr {
		t.Fatalf("%s: expected isError, got %v", name, result)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s: no content", name)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// ─────────────────────────────────────────────────────────────────────────
// protocol
// ─────────────────────────────────────────────────────────────────────────

func TestInitializeNegotiates(t *testing.T) {
	h := newHarness(t)

	res := h.rpc(t, "1", "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}`)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("protocolVersion = %v", res["protocolVersion"])
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != "srcos" || info["version"] != "test-version" {
		t.Fatalf("serverInfo = %v", info)
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("capabilities = %v", caps)
	}
	if inst, _ := res["instructions"].(string); !strings.Contains(inst, "submit") || !strings.Contains(inst, "Read surface") {
		t.Fatalf("instructions = %v", res["instructions"])
	}

	// A version we do not speak gets the version we do (the spec's rule).
	res = h.rpc(t, "2", "initialize", `{"protocolVersion":"1999-01-01"}`)
	if res["protocolVersion"] != ProtocolVersion {
		t.Fatalf("unknown version negotiated to %v", res["protocolVersion"])
	}

	// An older revision we do support is echoed back.
	res = h.rpc(t, "3", "initialize", `{"protocolVersion":"2024-11-05"}`)
	if res["protocolVersion"] != "2024-11-05" {
		t.Fatalf("supported version not echoed: %v", res["protocolVersion"])
	}
}

func TestToolsListDeclaresSchemas(t *testing.T) {
	h := newHarness(t)
	res := h.rpc(t, "1", "tools/list", `{}`)
	tools, _ := res["tools"].([]any)
	if len(tools) != len(readTools()) {
		t.Fatalf("tools = %d, want %d", len(tools), len(readTools()))
	}

	want := map[string]bool{
		"srcos_list_tools": false, "srcos_describe_tool": false, "srcos_list_storages": false,
		"srcos_list_paths": false, "srcos_list_instances": false, "srcos_task_status": false,
		"srcos_task_logs": false, "srcos_list_artifacts": false, "srcos_read_file": false,
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("tool entry = %T", raw)
		}
		name, _ := tool["name"].(string)
		if _, expected := want[name]; !expected {
			t.Errorf("unexpected tool %q", name)
			continue
		}
		want[name] = true
		schema, _ := tool["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Errorf("%s: inputSchema is not an object schema: %v", name, schema)
		}
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Errorf("%s: this phase is read-only, so readOnlyHint must be set", name)
		}
		if desc, _ := tool["description"].(string); desc == "" {
			t.Errorf("%s: a tool without a description is unusable by a model", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %s is missing from tools/list", name)
		}
	}
}

func TestNotificationsAndUnknownMethods(t *testing.T) {
	h := newHarness(t)

	// A notification gets 202 and no body — including one we do not know.
	rec, _ := h.call(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("notification: %d %q", rec.Code, rec.Body)
	}
	rec, _ = h.call(t, `{"jsonrpc":"2.0","method":"notifications/something-else","params":{}}`)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Fatalf("unknown notification: %d %q", rec.Code, rec.Body)
	}

	// An unimplemented *method* is an error, and says what this server does.
	_, body := h.call(t, `{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method = %v", body)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "tools only") {
		t.Fatalf("message = %v", msg)
	}

	// Ping is the liveness check and answers with an empty result.
	res := h.rpc(t, "8", "ping", `{}`)
	if len(res) != 0 {
		t.Fatalf("ping = %v", res)
	}
}

func TestMalformedRequests(t *testing.T) {
	h := newHarness(t)

	rec, body := h.call(t, `{"jsonrpc":"2.0","id":1,"method":`)
	if errObj, _ := body["error"].(map[string]any); errObj == nil || errObj["code"].(float64) != codeParseError {
		t.Fatalf("parse error = %d %v", rec.Code, body)
	}

	rec, body = h.call(t, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("batch = %d", rec.Code)
	}
	if errObj, _ := body["error"].(map[string]any); errObj == nil ||
		!strings.Contains(errObj["message"].(string), "batching") {
		t.Fatalf("batch error = %v", body)
	}

	_, body = h.call(t, `{"jsonrpc":"1.0","id":1,"method":"ping"}`)
	if errObj, _ := body["error"].(map[string]any); errObj == nil || errObj["code"].(float64) != codeInvalidRequest {
		t.Fatalf("bad jsonrpc version = %v", body)
	}

	// An unknown tool is a protocol error: the call itself was malformed.
	_, body = h.call(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`)
	if errObj, _ := body["error"].(map[string]any); errObj == nil || errObj["code"].(float64) != codeInvalidParams {
		t.Fatalf("unknown tool = %v", body)
	}
}

// An oversized body is refused as one JSON-RPC message, not as a bare 413.
func TestOversizedBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	huge := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"srcos_read_file","arguments":{"path":"` +
		strings.Repeat("x", maxRequestBytes+16) + `"}}}`
	rec, body := h.call(t, huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", rec.Code)
	}
	if errObj, _ := body["error"].(map[string]any); errObj == nil || errObj["code"].(float64) != codeInvalidRequest {
		t.Fatalf("body = %v", body)
	}
}

func TestTransportRejectsNonPosts(t *testing.T) {
	h := newHarness(t)
	req := httptest.NewRequest("GET", Endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t)

	post := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", Endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post("")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("no credential: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	if !strings.Contains(rec.Body.String(), "agent token") {
		t.Fatalf("the refusal should say what is required: %s", rec.Body)
	}
	if rec := post("Bearer srcos_zzzzzzzz.AAAA"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d", rec.Code)
	}
	// A browser session is not a credential here: MCP is the program surface.
	if rec := post("Bearer " + h.token); rec.Code != http.StatusOK {
		t.Fatalf("valid token: %d %s", rec.Code, rec.Body)
	}
}

func TestCrossOriginIsRejected(t *testing.T) {
	req := httptest.NewRequest("POST", Endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	// httptest sets Host: example.com; this Origin is another host.
	req.Header.Set("Origin", "http://evil.example")
	rec := httptest.NewRecorder()
	newHarness(t).handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d %s", rec.Code, rec.Body)
	}
}

// A token whose account was deleted must stop working, the same way it does on
// the REST API: the check lives in the token store, so both front-ends have it.
func TestTokenOfDeletedUserIsRejected(t *testing.T) {
	h := newHarness(t)
	if err := os.Remove(config.UserConfigPath(h.configDir, "alice")); err != nil {
		t.Fatal(err)
	}
	// The registry refreshes on an interval (10s); the gateway's scan loop
	// would pick this up on its own, and the test does not wait for it.
	h.registry.Reload()

	req := httptest.NewRequest("POST", Endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("deleted user = %d %s", rec.Code, rec.Body)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// the write half (ADR-019 phase 2)
// ─────────────────────────────────────────────────────────────────────────

// toolNames reads tools/list and returns the names offered to this credential.
func (h *harness) toolNames(t *testing.T) map[string]map[string]any {
	t.Helper()
	res := h.rpc(t, "1", "tools/list", `{}`)
	list, _ := res["tools"].([]any)
	out := map[string]map[string]any{}
	for _, raw := range list {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		out[name] = tool
	}
	return out
}

func TestWriteToolsAreOfferedOnlyToSubmitTokens(t *testing.T) {
	h := newHarness(t)

	// A read-only token sees exactly the read surface it has.
	read := h.toolNames(t)
	for _, name := range []string{"srcos_submit_job", "srcos_cancel_instance", "srcos_run_flow"} {
		if _, ok := read[name]; ok {
			t.Errorf("a read-only token was offered %s", name)
		}
	}
	if tool, ok := read["srcos_list_tools"]; !ok {
		t.Fatal("the read catalogue is missing")
	} else if ann, _ := tool["annotations"].(map[string]any); ann["readOnlyHint"] != true {
		t.Errorf("read tool annotations = %v", ann)
	}

	// A submit token sees them, annotated as writes.
	var write map[string]map[string]any
	h.useToken(h.submitToken, func() { write = h.toolNames(t) })
	for _, name := range []string{"srcos_submit_job", "srcos_cancel_instance", "srcos_run_flow"} {
		tool, ok := write[name]
		if !ok {
			t.Fatalf("a submit token was not offered %s", name)
		}
		ann, _ := tool["annotations"].(map[string]any)
		if ann["readOnlyHint"] != false {
			t.Errorf("%s annotations = %v, want readOnlyHint=false", name, ann)
		}
	}
	// The read tools are still there.
	if _, ok := write["srcos_list_instances"]; !ok {
		t.Error("the read catalogue disappeared for a submit token")
	}
}

func TestSubmitJobThroughMCP(t *testing.T) {
	h := newHarness(t)

	var res map[string]any
	h.useToken(h.submitToken, func() {
		out := h.callTool(t, "srcos_submit_job", `{"tool":"demo","params":{"ref":"/data","samples":"S1"},"name":"from mcp"}`)
		res, _ = out.(map[string]any)
	})
	if res["jobId"] == "" || res["jobId"] == nil {
		t.Fatalf("submit result = %v", res)
	}
	if res["instanceId"] == nil {
		t.Errorf("an agent's submit must start the run: %v", res)
	}
	// The submission is a real drop-box entry, visible to the read side.
	if ids, _ := h.callTool(t, "srcos_list_instances", `{}`).(map[string]any); ids == nil {
		t.Fatalf("list_instances = %v", ids)
	}
}

func TestWriteToolRefusedWithoutTheSubmitScope(t *testing.T) {
	h := newHarness(t)
	// The read token knows the name (the listing hides it, the name is guessable)
	// and must still be refused.
	text := h.callToolError(t, "srcos_submit_job", `{"tool":"demo","params":{"ref":"/data","samples":"S1"}}`)
	if !strings.Contains(text, "submit") {
		t.Errorf("refusal = %q", text)
	}
}

func TestSubmitHonoursTheToolAllowlist(t *testing.T) {
	h := newHarness(t)
	// The submit token's allowlist names only "demo", so "web" is refused — and
	// "web" is a service anyway, which the execute layer rejects separately.
	text := h.callToolError(t, "srcos_submit_job", `{"tool":"web","params":{}}`)
	if text == "" {
		t.Fatal("no refusal text")
	}
}

func TestCancelThroughMCP(t *testing.T) {
	h := newHarness(t)
	h.useToken(h.submitToken, func() {
		text := h.callToolError(t, "srcos_cancel_instance", `{"instance":"alice-demo-nope"}`)
		if !strings.Contains(text, "not found") {
			t.Errorf("cancel of an unknown instance = %q", text)
		}
	})
}
