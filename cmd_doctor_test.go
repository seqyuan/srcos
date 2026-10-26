package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doctorFixture(t *testing.T) (configDir, toolsDir, flowsDir string) {
	t.Helper()
	base := t.TempDir()
	configDir = filepath.Join(base, "config")
	toolsDir = filepath.Join(base, "tools")
	flowsDir = filepath.Join(base, "flows")
	for _, d := range []string{configDir, toolsDir, flowsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(toolsDir, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `schemaVersion: 1
id: demo
name: Demo
kind: task
backend: local
sandbox: none
entry: work.sh
resources: {cpu: 1, memory: "256Mi", walltime: "0:01:00"}
`
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return configDir, toolsDir, flowsDir
}

func findCheck(checks []doctorCheck, name string) (doctorCheck, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return doctorCheck{}, false
}

func TestDoctorReportCleanDeployment(t *testing.T) {
	configDir, toolsDir, flowsDir := doctorFixture(t)
	checks := doctorReport(configDir, toolsDir, flowsDir)
	if code := doctorExitCode(checks); code != 0 {
		t.Fatalf("clean deployment exit = %d: %+v", code, checks)
	}
	for _, name := range []string{"config", "data", "claims", "tools", "storages", "environments", "flows"} {
		c, ok := findCheck(checks, name)
		if !ok {
			t.Errorf("missing check %s: %+v", name, checks)
			continue
		}
		if c.Status == "fail" {
			t.Errorf("%s failed: %s", name, c.Detail)
		}
	}
}

func TestDoctorReportFailsOnAnUnreachableStorage(t *testing.T) {
	configDir, toolsDir, flowsDir := doctorFixture(t)
	body := `storages:
  - id: gone
    name: Gone
    type: posix
    host_root: /definitely/not/here
    sandbox_path: /data
    mode: ro
`
	if err := os.WriteFile(filepath.Join(configDir, "storages.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := doctorReport(configDir, toolsDir, flowsDir)
	c, ok := findCheck(checks, "storages")
	if !ok || c.Status != "fail" {
		t.Fatalf("storages = %+v", c)
	}
	if doctorExitCode(checks) != 1 {
		t.Fatal("a failed check must exit 1")
	}
}

func TestDoctorReportFailsOnABadToolAndFlow(t *testing.T) {
	configDir, toolsDir, flowsDir := doctorFixture(t)
	if err := os.WriteFile(filepath.Join(toolsDir, "demo", "tool.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	checks := doctorReport(configDir, toolsDir, flowsDir)
	if c, ok := findCheck(checks, "tools"); !ok || c.Status != "fail" {
		t.Fatalf("tools = %+v", c)
	}
	if doctorExitCode(checks) != 1 {
		t.Fatal("a failed check must exit 1")
	}

	// A flow that references a missing tool is a deployment error.
	configDir, toolsDir, flowsDir = doctorFixture(t)
	flowDir := filepath.Join(flowsDir, "bad")
	if err := os.MkdirAll(flowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	flowYAML := `schemaVersion: 1
id: bad
version: 1.0.0
name: Bad
nodes:
  - {id: run, tool: missing}
bindings: []
expose: []
`
	if err := os.WriteFile(filepath.Join(flowDir, "flow.yaml"), []byte(flowYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	checks = doctorReport(configDir, toolsDir, flowsDir)
	if c, ok := findCheck(checks, "flows"); !ok || c.Status != "fail" || !strings.Contains(c.Detail, "missing") {
		t.Fatalf("flows = %+v", c)
	}
}
