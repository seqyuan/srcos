package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeRuntime writes an executable that prints script and exits 0.
func writeFakeRuntime(t *testing.T, dir, name, script string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestApptainerProbeRejectsSameNamedBinary pins the bug Ubuntu's pygame game
// exposed: a binary called `singularity` that exits 0 on `--version` is not a
// container runtime. The probe must read the output, not just the exit code.
func TestApptainerProbeRejectsSameNamedBinary(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntime(t, dir, "singularity",
		"#!/bin/sh\necho 'Singularity 1.00 (commit: deadbeef)'\necho '1.00'\nexit 0\n")
	t.Setenv("PATH", dir)
	resetApptainerProbe()
	t.Cleanup(resetApptainerProbe)

	_, ok, why := ApptainerProbe()
	if ok {
		t.Fatal("a same-named binary that only exits 0 must not be accepted as a runtime")
	}
	if !strings.Contains(why, "does not look like") {
		t.Fatalf("reason should say the binary is not a runtime, got %q", why)
	}
}

// TestApptainerProbeAcceptsRealVersionBanner is the other half: a real banner
// (what Apptainer and Singularity CE actually print) must still pass.
func TestApptainerProbeAcceptsRealVersionBanner(t *testing.T) {
	dir := t.TempDir()
	want := writeFakeRuntime(t, dir, "apptainer",
		"#!/bin/sh\necho 'apptainer version 1.3.3'\n")
	t.Setenv("PATH", dir)
	resetApptainerProbe()
	t.Cleanup(resetApptainerProbe)

	p, ok, why := ApptainerProbe()
	if !ok {
		t.Fatalf("a real version banner must be accepted, got %q", why)
	}
	if p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}
}

// TestApptainerProbeAcceptsSingularityBanner covers the fallback binary, whose
// banner differs only in the name.
func TestApptainerProbeAcceptsSingularityBanner(t *testing.T) {
	dir := t.TempDir()
	writeFakeRuntime(t, dir, "singularity",
		"#!/bin/sh\necho 'singularity version 4.1.0'\n")
	t.Setenv("PATH", dir)
	resetApptainerProbe()
	t.Cleanup(resetApptainerProbe)

	if _, ok, why := ApptainerProbe(); !ok {
		t.Fatalf("a real Singularity CE banner must be accepted, got %q", why)
	}
}
