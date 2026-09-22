package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The flow API is what the canvas talks to, so the rules it enforces are the
// rules the canvas appears to enforce. These tests pin both halves: an
// administrator can read and save a flow, and nothing else can.

func newFlowHarness(t *testing.T) *adminHarness {
	t.Helper()
	h := newAdminHarness(t)
	h.opts.FlowsDir = t.TempDir()
	// A tool with a directory input and a directory output, so the canvas tests
	// can exercise a legal wire, an illegal one, and an unbound required input.
	countDir := filepath.Join(h.opts.ToolsDir, "count")
	if err := os.MkdirAll(countDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schemaVersion: 1
id: count
version: 0.1.0
name: Count
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: fastq_dir, type: directory, required: true}
    - {name: sample_id, type: string, required: true}
  outputs:
    - {name: outs, type: directory}
resources: {cpu: 1, memory: "512Mi", walltime: "0:10:00"}
`
	if err := os.WriteFile(filepath.Join(countDir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(countDir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFlowFixture(t, h.opts.FlowsDir, "scrna", `schemaVersion: 1
id: scrna
version: 0.1.0
name: "小流程"
nodes:
  - {id: count, tool: count}
expose:
  - {node: count, input: fastq_dir, from: sample.fastq_dir}
  - {node: count, input: sample_id, from: sample.sample_id}
`)
	writeFlowFixture(t, h.opts.FlowsDir, "broken", `schemaVersion: 1
id: broken
version: 0.1.0
name: "坏流程"
nodes:
  - {id: count, tool: count}
`)
	return h
}

// writeFlowFixture writes a flow package into a flows root.
func writeFlowFixture(t *testing.T, root, id, body string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flow.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdminFlowListReportsValidity(t *testing.T) {
	h := newFlowHarness(t)
	rec := h.asAdmin("GET", "/api/admin/flows", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"scrna"`) || !strings.Contains(body, `"valid":true`) {
		t.Fatalf("a valid flow must be listed as valid: %s", body)
	}
	// A flow that does not validate is still listed — with its problem, because
	// "which flow is broken" is exactly what an operator opens the console for.
	if !strings.Contains(body, `"id":"broken"`) || !strings.Contains(body, `"problem"`) {
		t.Fatalf("a broken flow must be listed with its problem: %s", body)
	}
	// The canvas needs the sample columns to offer them as parameter sources.
	if !strings.Contains(body, `"sampleColumns":["fastq_dir","sample_id"]`) {
		t.Fatalf("sample columns missing: %s", body)
	}
}

func TestAdminFlowGetCarriesToolsAndVerdict(t *testing.T) {
	h := newFlowHarness(t)
	rec := h.asAdmin("GET", "/api/admin/flows/scrna", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	// The tool catalogue travels with the flow: the canvas needs the interfaces
	// to draw ports and to offer parameter sources.
	for _, want := range []string{`"flow":`, `"tools":`, `"id":"count"`, `"interface"`, `"valid":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("editor view is missing %s", want)
		}
	}
	if rec := h.asAdmin("GET", "/api/admin/flows/nope", ""); rec.Code != 404 {
		t.Fatalf("unknown flow = %d", rec.Code)
	}
	// An id that is not a slug never reaches the filesystem.
	if rec := h.asAdmin("GET", "/api/admin/flows/..%2F..%2Fetc", ""); rec.Code == 200 {
		t.Fatalf("a path-shaped id must be refused: %s", rec.Body)
	}
}

func TestAdminFlowValidateDoesNotWrite(t *testing.T) {
	h := newFlowHarness(t)
	// A candidate with a wire from an output to a scalar input: the type rules
	// live in internal/flow, and the canvas asks rather than reimplementing them.
	candidate := `{"schemaVersion":1,"id":"scrna","version":"0.1.0","name":"x",
		"nodes":[{"id":"count","tool":"count"}],
		"bindings":[{"from":"count.outputs.outs","to":"count.inputs.sample_id"}],
		"expose":[]}`
	rec := h.asAdmin("POST", "/api/admin/flows/validate", candidate)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"valid":false`) {
		t.Fatalf("an illegal wire must be reported invalid: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "only paths travel") {
		t.Fatalf("the reason must be the contract's: %s", rec.Body)
	}
	// And nothing was written.
	before, _ := os.ReadFile(filepath.Join(h.opts.FlowsDir, "scrna", "flow.yaml"))
	if !strings.Contains(string(before), "小流程") {
		t.Fatalf("validate found a way to write: %s", before)
	}

	// A valid candidate says so.
	ok := `{"schemaVersion":1,"id":"scrna","version":"0.1.0","name":"x",
		"nodes":[{"id":"count","tool":"count"}],
		"expose":[{"node":"count","input":"fastq_dir","from":"sample.fastq_dir"},
		          {"node":"count","input":"sample_id","from":"sample.sample_id"}]}`
	if rec := h.asAdmin("POST", "/api/admin/flows/validate", ok); !strings.Contains(rec.Body.String(), `"valid":true`) {
		t.Fatalf("a valid candidate must validate: %s", rec.Body)
	}
}

func TestAdminFlowSaveValidatesAndPersists(t *testing.T) {
	h := newFlowHarness(t)

	// A broken candidate is refused before it can reach the disk: a flow that
	// does not validate would fail later, in the scheduler, far from the person
	// who drew it.
	broken := `{"schemaVersion":1,"id":"scrna","version":"0.1.0","name":"x",
		"nodes":[{"id":"count","tool":"nope"}]}`
	rec := h.asAdmin("PUT", "/api/admin/flows/scrna", broken)
	if rec.Code != 400 {
		t.Fatalf("an invalid flow must not be saved: %d %s", rec.Code, rec.Body)
	}

	// A valid one is written, and the CLI can read it back.
	good := `{"schemaVersion":1,"id":"scrna","version":"0.2.0","name":"画布保存的流程",
		"nodes":[{"id":"count","tool":"count"}],
		"expose":[{"node":"count","input":"fastq_dir","from":"sample.fastq_dir"},
		          {"node":"count","input":"sample_id","from":"sample.sample_id"}]}`
	if rec := h.asAdmin("PUT", "/api/admin/flows/scrna", good); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	data, err := os.ReadFile(filepath.Join(h.opts.FlowsDir, "scrna", "flow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "画布保存的流程") || !strings.Contains(string(data), "# SRCOS 流程定义") {
		t.Fatalf("the saved file does not look like a flow.yaml:\n%s", data)
	}
	// The id in the path wins over the body, so a file cannot disagree with its
	// own directory name.
	if rec := h.asAdmin("PUT", "/api/admin/flows/other", good); rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	data, _ = os.ReadFile(filepath.Join(h.opts.FlowsDir, "other", "flow.yaml"))
	if !strings.Contains(string(data), "id: other") {
		t.Fatalf("the directory name is the identity: %s", data)
	}
}

// The flow endpoints are admin-only like the rest of the management surface.
func TestAdminFlowEndpointsAreAdminOnly(t *testing.T) {
	h := newFlowHarness(t)
	for _, tc := range []struct{ method, url, body string }{
		{"GET", "/api/admin/flows", ""},
		{"GET", "/api/admin/flows/scrna", ""},
		{"POST", "/api/admin/flows/validate", `{}`},
		{"PUT", "/api/admin/flows/scrna", `{}`},
	} {
		if rec := h.as(tc.method, tc.url, tc.body, "alice"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a non-admin = %d", tc.method, tc.url, rec.Code)
		}
	}
}

// Without a flows directory the canvas says so instead of pretending there are
// no flows.
func TestAdminFlowsUnavailableWithoutADirectory(t *testing.T) {
	h := newAdminHarness(t) // no FlowsDir
	rec := h.asAdmin("GET", "/api/admin/flows", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}
