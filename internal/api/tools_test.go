package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
)

// toolsHarness wires a handler with a tool package, a storage root and a
// session, so the tool/storage API can be exercised without a server.
type toolsHarness struct {
	*Handler
	configDir string
	toolsDir  string
	storageID string
	root      string
	cookie    string
}

const demoManifest = `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
entry: work.sh
interface:
  inputs:
    - {name: ref, type: path, from: data, select: directory, required: true}
    - {name: label, type: string, default: hi}
resources: {cpu: 2, memory: "2Gi", walltime: "0:10:00"}
requires_storages: [data]
`

const serviceManifest = `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080}
lifecycle: {max_lifetime: "1h"}
`

func newToolsHarness(t *testing.T, grants GrantChecker) *toolsHarness {
	t.Helper()
	configDir := t.TempDir()
	cfgPath := config.UserConfigPath(configDir, "alice")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	pw := strings.Repeat("1", 64)
	if err := os.WriteFile(cfgPath, []byte("auth:\n  password_hash: \""+pw+"\"\nservices: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := config.NewUserRegistry(configDir)
	reg.Reload()

	toolsDir := t.TempDir()
	for id, manifest := range map[string]string{"demo": demoManifest, "web": serviceManifest} {
		dir := filepath.Join(toolsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A storage root with a nested directory and a file, so filters and descent
	// have something to act on.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ref", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, err := storage.New([]storage.Storage{{
		ID: "data", Name: "Data", Kind: storage.KindPosix,
		HostRoot: root, SandboxPath: "/data", Mode: storage.ReadOnly,
	}})
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandlerWithOptions(reg, "testsecret", Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		Storages:  provider,
		Grants:    grants,
	})
	return &toolsHarness{
		Handler:   h,
		configDir: configDir,
		toolsDir:  toolsDir,
		storageID: "data",
		root:      root,
		cookie:    auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(pw), false),
	}
}

func (h *toolsHarness) get(t *testing.T, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", h.cookie)
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, req) {
		t.Fatalf("ServeHTTP declined %s", url)
	}
	return rec
}

func TestToolsListAndDescribe(t *testing.T) {
	h := newToolsHarness(t, nil)

	rec := h.get(t, "/api/tools")
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"demo"`) || !strings.Contains(rec.Body.String(), `"web"`) {
		t.Fatalf("catalogue missing tools: %s", rec.Body)
	}

	rec = h.get(t, "/api/tools/demo")
	if rec.Code != 200 {
		t.Fatalf("describe: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// The machine-readable interface is the whole point: an agent, a canvas and
	// a form all read these fields, so their spelling is a contract.
	for _, want := range []string{`"interface"`, `"ref"`, `"type":"path"`, `"from":"data"`, `"select":"directory"`, `"storages"`} {
		if !strings.Contains(body, want) {
			t.Errorf("describe missing %s:\n%s", want, body)
		}
	}
	// json tags must produce the snake/lowercase spelling, not Go field names.
	if strings.Contains(body, `"Inputs"`) {
		t.Errorf("interface is leaking Go field names: %s", body)
	}
}

func TestToolsDescribeUnknown(t *testing.T) {
	h := newToolsHarness(t, nil)
	if rec := h.get(t, "/api/tools/nope"); rec.Code != 404 {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestPathsDerivesStorageFromTheTool is the ADR-020 closure enforced at the API
// edge: the browsable storage comes from the tool's own interface, not from the
// caller.
func TestPathsDerivesStorageFromTheTool(t *testing.T) {
	h := newToolsHarness(t, nil)

	rec := h.get(t, "/api/paths?tool=demo&input=ref")
	if rec.Code != 200 {
		t.Fatalf("paths: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"storage":"data"`) || !strings.Contains(body, `"path":"/data"`) {
		t.Fatalf("unexpected listing: %s", body)
	}
	// Entries carry sandbox paths, not host paths, because that is the value a
	// tool receives.
	if !strings.Contains(body, `"/data/ref"`) {
		t.Fatalf("entries should carry sandbox paths: %s", body)
	}
	// The input declares select: directory, so the file must not be offered.
	if strings.Contains(body, "readme.txt") {
		t.Fatalf("a directory-typed input offered a file: %s", body)
	}
}

func TestPathsRejectsUndeclaredStorage(t *testing.T) {
	h := newToolsHarness(t, nil)
	// A caller trying to widen its own reach: the storage is not in the tool's
	// `from`, so it is refused even though the provider knows it.
	rec := h.get(t, "/api/paths?tool=demo&input=ref&storage=data")
	if rec.Code != 200 {
		t.Fatalf("the declared storage should be allowed: %d %s", rec.Code, rec.Body)
	}
	rec = h.get(t, "/api/paths?tool=demo&input=ref&storage=other")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body)
	}
}

func TestPathsRejectsEscapesAndUnknownInputs(t *testing.T) {
	h := newToolsHarness(t, nil)
	cases := []struct {
		url    string
		status int
	}{
		{"/api/paths?tool=demo&input=ref&path=/data/../..", 400},
		{"/api/paths?tool=demo&input=ref&path=/etc", 400},
		{"/api/paths?tool=demo&input=nope", 404},
		{"/api/paths?tool=nope&input=ref", 404},
		{"/api/paths?tool=demo", 400},          // input is required
		{"/api/paths?input=ref", 400},          // tool is required
		{"/api/paths?tool=web&input=ref", 404}, // service declares no storages
	}
	for _, tc := range cases {
		rec := h.get(t, tc.url)
		if rec.Code != tc.status {
			t.Errorf("%s: status = %d, want %d (%s)", tc.url, rec.Code, tc.status, rec.Body)
		}
	}
}

func TestPathsHonoursSelectFilter(t *testing.T) {
	h := newToolsHarness(t, nil)
	rec := h.get(t, "/api/paths?tool=demo&input=ref&select=file")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "readme.txt") {
		t.Fatalf("file filter dropped the file: %s", body)
	}
	// Assert on the entry path, not the bare name: "ref" also appears as
	// `"input":"ref"` in the envelope.
	if strings.Contains(body, `"/data/ref"`) {
		t.Fatalf("file filter kept a directory: %s", body)
	}
}

// TestPathsNeedsNoPathsProvider: without a StorageProvider the endpoint answers
// 503 rather than pretending there is no data.
func TestPathsWithoutProvider(t *testing.T) {
	configDir := t.TempDir()
	cfgPath := config.UserConfigPath(configDir, "alice")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	pw := strings.Repeat("1", 64)
	if err := os.WriteFile(cfgPath, []byte("auth:\n  password_hash: \""+pw+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := config.NewUserRegistry(configDir)
	reg.Reload()
	h := NewHandlerWithOptions(reg, "testsecret", Options{ConfigDir: configDir})

	req := httptest.NewRequest("GET", "/api/paths?tool=x&input=y", nil)
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "alice", auth.SessionRev(pw), false))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestSubmitJobWritesTheDropBox(t *testing.T) {
	h := newToolsHarness(t, nil)
	body := `{"tool":"demo","name":"my run","params":{"ref":"/data/ref"},"tags":{"a":"1"}}`

	rec := apiPost(t, h.Handler, "/api/jobs", body, "http://gw:30152")
	if rec.Code != 201 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"jobId"`) {
		t.Fatalf("no job id: %s", rec.Body)
	}

	// The submission is a file in the drop-box, which is the whole design: no
	// daemon, no socket, replays across a restart.
	jobsDir := config.JobsDir(h.configDir, "alice", "demo")
	entries, err := os.ReadDir(jobsDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one job directory, got %v (%v)", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(jobsDir, entries[0].Name(), "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"my run"`) || !strings.Contains(string(data), `"ref": "/data/ref"`) {
		t.Fatalf("job.json = %s", data)
	}
}

func TestSubmitJobValidatesAgainstTheInterface(t *testing.T) {
	h := newToolsHarness(t, nil)
	cases := map[string]string{
		"undeclared param":    `{"tool":"demo","params":{"bogus":1}}`,
		"missing required":    `{"tool":"demo","params":{}}`,
		"resource escalation": `{"tool":"demo","params":{"ref":"/data/ref"},"resources":{"cpu":64}}`,
		"unknown tool":        `{"tool":"nope","params":{}}`,
		"no tool":             `{"params":{}}`,
		"service, not job":    `{"tool":"web","params":{}}`,
	}
	for name, body := range cases {
		rec := apiPost(t, h.Handler, "/api/jobs", body, "http://gw:30152")
		if rec.Code < 400 {
			t.Errorf("%s: expected a rejection, got %d %s", name, rec.Code, rec.Body)
		}
	}
}

func TestSubmitJobRejectsCrossOrigin(t *testing.T) {
	h := newToolsHarness(t, nil)
	rec := apiPost(t, h.Handler, "/api/jobs", `{"tool":"demo","params":{"ref":"/data/ref"}}`, "http://evil.example")
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (CSRF guard)", rec.Code)
	}
}

func TestListJobsIsScopedToTheUser(t *testing.T) {
	h := newToolsHarness(t, nil)

	// Two records, one of them someone else's. They go through the runtime
	// package's public API, so the test asserts on the same on-disk shape
	// production reads.
	for _, rec := range []struct{ user, id string }{
		{"alice", "alice-demo-a"},
		{"bob", "bob-demo-b"},
	} {
		inst := &runtime.Instance{
			ID: rec.id, User: rec.user, Tool: "demo", Kind: "task",
			State: runtime.StateSucceeded, Backend: "local",
		}
		if err := runtime.SaveInstance(runtime.InstancePath(h.configDir, rec.id), inst); err != nil {
			t.Fatal(err)
		}
	}

	res := h.get(t, "/api/jobs")
	if res.Code != 200 {
		t.Fatalf("status = %d %s", res.Code, res.Body)
	}
	body := res.Body.String()
	if !strings.Contains(body, "alice-demo-a") {
		t.Errorf("own instance missing: %s", body)
	}
	if strings.Contains(body, "bob-demo-b") {
		t.Errorf("another user's instance leaked: %s", body)
	}
}

func TestGrantsFilterTheCatalogue(t *testing.T) {
	h := newToolsHarness(t, denyAll{})
	res := h.get(t, "/api/tools")
	if res.Code != 200 {
		t.Fatalf("status = %d", res.Code)
	}
	if strings.Contains(res.Body.String(), `"demo"`) {
		t.Fatalf("a denied tool is still listed: %s", res.Body)
	}
	// And describing it must say "not authorized" rather than "unknown",
	// because the difference is what the user asks an admin about.
	res = h.get(t, "/api/tools/demo")
	if res.Code != 404 || !strings.Contains(res.Body.String(), "not authorized") {
		t.Fatalf("status = %d body = %s", res.Code, res.Body)
	}
}

type denyAll struct{}

func (denyAll) Allowed(_, _ string) bool         { return false }
func (denyAll) IsAdmin(string) bool              { return false }
func (denyAll) QuotaFor(_, _ string) grant.Quota { return grant.Quota{} }

// quotaPolicy grants one tool with a ceiling, for the quota tests.
type quotaPolicy struct {
	tool  string
	quota grant.Quota
}

func (p quotaPolicy) Allowed(_, toolID string) bool { return toolID == p.tool }
func (p quotaPolicy) IsAdmin(string) bool           { return false }
func (p quotaPolicy) QuotaFor(_, toolID string) grant.Quota {
	if toolID == p.tool {
		return p.quota
	}
	return grant.Quota{}
}

// TestSubmitJobEnforcesQuota pins the aggregate ceiling: a request inside the
// tool's own declaration can still be refused because of who is asking.
func TestSubmitJobEnforcesQuota(t *testing.T) {
	h := newToolsHarness(t, quotaPolicy{tool: "demo", quota: grant.Quota{MaxCPU: 2}})

	// demo declares cpu: 2, so this fits exactly.
	ok := apiPost(t, h.Handler, "/api/jobs", `{"tool":"demo","params":{"ref":"/data/ref"}}`, "http://gw:30152")
	if ok.Code != 201 {
		t.Fatalf("a request within quota was refused: %d %s", ok.Code, ok.Body)
	}

	// Now one that would exceed it. The tool allows 2 cores, so the loop is
	// closed by the quota, not the tool ceiling.
	h2 := newToolsHarness(t, quotaPolicy{tool: "demo", quota: grant.Quota{MaxInstances: 1}})
	first := apiPost(t, h2.Handler, "/api/jobs", `{"tool":"demo","params":{"ref":"/data/ref"}}`, "http://gw:30152")
	if first.Code != 201 {
		t.Fatalf("first submission: %d %s", first.Code, first.Body)
	}

	// A live instance now holds the only slot. It has to be recorded as
	// non-terminal for the quota to see it.
	inst := &runtime.Instance{
		ID: "alice-demo-live", User: "alice", Tool: "demo", Kind: "task",
		State: runtime.StateRunning, Backend: "local",
		RequestedCPU: 2, RequestedMemory: "2Gi",
	}
	if err := runtime.SaveInstance(runtime.InstancePath(h2.configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}

	second := apiPost(t, h2.Handler, "/api/jobs", `{"tool":"demo","params":{"ref":"/data/ref"}}`, "http://gw:30152")
	if second.Code != 403 {
		t.Fatalf("status = %d, want 403 (quota): %s", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), "quota") {
		t.Fatalf("the message should name the quota: %s", second.Body)
	}
}
