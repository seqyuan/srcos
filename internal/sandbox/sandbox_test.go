package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveMapsSandboxToHost(t *testing.T) {
	spec := &Spec{}
	if err := spec.Add(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite, Origin: "builtin"}); err != nil {
		t.Fatal(err)
	}
	if err := spec.Add(Mount{HostPath: "/host/tool", SandboxPath: "/tool", Mode: ReadOnly, Origin: "tool"}); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"/workspace":              "/host/ws",
		"/workspace/out":          "/host/ws/out",
		"/workspace/out/S001.txt": "/host/ws/out/S001.txt",
		"/tool/work.sh":           "/host/tool/work.sh",
	}
	for in, want := range cases {
		got, _, err := spec.Resolve(in)
		if err != nil {
			t.Errorf("Resolve(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveRejectsOutsideMounts(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite})
	for _, bad := range []string{"/", "/etc/passwd", "/home/seqyuan", "/workspaceX", "/workspac"} {
		if _, _, err := spec.Resolve(bad); err == nil {
			t.Errorf("Resolve(%q) should fail", bad)
		}
	}
}

// TestResolveRejectsTraversal covers the classic escape: a `..` segment that
// would leave the mount after the join.
func TestResolveRejectsTraversal(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite})
	// filepath.Clean collapses the .. before the segment check, so these must
	// either be normalized inside the mount or rejected outright.
	for _, in := range []string{"/workspace/../etc/passwd", "/workspace/../../etc/passwd"} {
		host, _, err := spec.Resolve(in)
		if err == nil {
			if !isUnderFS(host, "/host/ws") {
				t.Errorf("Resolve(%q) escaped to %q", in, host)
			}
		}
	}
}

// TestAddRejectsOverlap pins the invariant that makes MountSpec the isolation
// boundary: a nested mount is how a sandbox silently gains a wider view.
func TestAddRejectsOverlap(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/share", SandboxPath: "/data/share", Mode: ReadOnly})

	err := spec.Add(Mount{HostPath: "/host/projA", SandboxPath: "/data/share/projA", Mode: ReadOnly})
	if err == nil {
		t.Fatal("a nested mount must be rejected, not resolved")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := spec.Add(Mount{HostPath: "/host/x", SandboxPath: "/data/share", Mode: ReadOnly}); err == nil {
		t.Fatal("duplicate sandbox path must be rejected")
	}
	if err := spec.Add(Mount{HostPath: "/host/y", SandboxPath: "/data", Mode: ReadOnly}); err == nil {
		t.Fatal("a parent mount must be rejected")
	}
}

func TestAddValidatesInput(t *testing.T) {
	spec := &Spec{}
	if err := spec.Add(Mount{HostPath: "", SandboxPath: "/x", Mode: ReadOnly}); err == nil {
		t.Error("empty host path must be rejected")
	}
	if err := spec.Add(Mount{HostPath: "/h", SandboxPath: "relative", Mode: ReadOnly}); err == nil {
		t.Error("relative sandbox path must be rejected")
	}
	if err := spec.Add(Mount{HostPath: "/h", SandboxPath: "/x", Mode: "rx"}); err == nil {
		t.Error("invalid mode must be rejected")
	}
}

func TestDisplayIsInverseOfResolve(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite})

	got, err := spec.Display("/host/ws/out/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/workspace/out/a.txt" {
		t.Fatalf("Display = %q", got)
	}
	if got, err := spec.Display("/host/ws"); err != nil || got != "/workspace" {
		t.Fatalf("Display(root) = %q, %v", got, err)
	}
	if _, err := spec.Display("/etc/passwd"); err == nil {
		t.Fatal("a host path outside every mount must not be displayable")
	}
}

// TestResolveExistingFollowsSymlinks covers the symlink escape: a link planted
// inside a writable mount must not redirect the lookup outside it.
func TestResolveExistingFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// ws/escape -> outside
	if err := os.Symlink(outside, filepath.Join(ws, "escape")); err != nil {
		t.Fatal(err)
	}

	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: ws, SandboxPath: "/workspace", Mode: ReadWrite})

	if _, err := spec.ResolveExisting("/workspace/escape/sneaky.txt"); err == nil {
		t.Fatal("a symlink leading outside the mount must be rejected")
	}
	// A legitimate in-mount path still resolves.
	if _, err := spec.ResolveExisting("/workspace/ok.txt"); err != nil {
		t.Fatalf("in-mount path should resolve even when the leaf is missing: %v", err)
	}
}

func TestResolveAllowsMissingLeaf(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite})
	// Resolve is structural: the caller may be about to create the path.
	if _, _, err := spec.Resolve("/workspace/does/not/exist"); err != nil {
		t.Fatalf("structural resolve should not require existence: %v", err)
	}
}

func TestBwrapArgvShape(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite})
	spec.MustAdd(Mount{HostPath: "/host/share", SandboxPath: "/data/share", Mode: ReadOnly})

	args := BwrapArgv(spec, BwrapOptions{
		Cwd:  JobRootPath("j1"),
		Env:  []string{"SRCOS_USER=alice", "SRCOS_PARAM_X=1"},
		Argv: []string{"bash", "/tool/work.sh"},
	})
	joined := strings.Join(args, " ")

	// Namespaces and a private /tmp
	for _, want := range []string{"--unshare-pid", "--unshare-ipc", "--unshare-uts", "--tmpfs /tmp", "--die-with-parent"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in argv", want)
		}
	}
	// Mounts carry their mode
	if !strings.Contains(joined, "--bind /host/ws /workspace") {
		t.Error("rw mount should use --bind")
	}
	if !strings.Contains(joined, "--ro-bind /host/share /data/share") {
		t.Error("ro mount should use --ro-bind")
	}
	// The host root is never bound wholesale: that would expose the OS user's
	// real home and every group-readable directory.
	if strings.Contains(joined, "--ro-bind / /") || strings.Contains(joined, "--bind / /") {
		t.Error("binding the whole host root defeats the sandbox")
	}
	// --setenv takes VAR VALUE as two arguments, not VAR=VALUE.
	if strings.Contains(joined, "--setenv SRCOS_USER=alice") {
		t.Error("--setenv must use the two-argument form")
	}
	if !strings.Contains(joined, "--setenv SRCOS_USER alice") {
		t.Error("expected --setenv SRCOS_USER alice")
	}
	// The command is separated and the cwd is set
	if !strings.Contains(joined, "--chdir /workspace/jobs/j1 -- bash /tool/work.sh") {
		t.Errorf("unexpected tail: %s", joined)
	}
	// Nothing from the host environment may leak in.
	if !strings.Contains(joined, "--clearenv") {
		t.Error("--clearenv is required so ambient variables cannot leak")
	}
}

func TestBwrapArgvAlwaysProvidesPath(t *testing.T) {
	args := BwrapArgv(&Spec{}, BwrapOptions{Argv: []string{"/bin/true"}})
	if !strings.Contains(strings.Join(args, " "), "--setenv PATH ") {
		t.Fatal("a cleared environment needs an explicit PATH")
	}
	if !strings.Contains(strings.Join(args, " "), PathToolBin) {
		t.Fatalf("PATH must include %s so tool-provided binaries are reachable", PathToolBin)
	}
}

func TestSandboxPathIsReserved(t *testing.T) {
	reserved := []string{
		"/usr/local/bin/ata",
		"/bin/sh",
		"/lib64/ld-linux.so",
		"/etc/passwd",
		"/etc/ssl/certs",
	}
	for _, p := range reserved {
		if !SandboxPathIsReserved(p) {
			t.Errorf("%q should be reserved (bubblewrap cannot create a mount point inside an already-bound path)", p)
		}
	}
	allowed := []string{
		PathToolBin + "/ata",
		"/opt/conda",
		"/data/share",
		"/probe", // a fresh top-level dir is fine
	}
	for _, p := range allowed {
		if SandboxPathIsReserved(p) {
			t.Errorf("%q should be usable as a mount target", p)
		}
	}
}

func TestHomePathAndJobRootPath(t *testing.T) {
	if got := HomePath("alice"); got != "/home/alice" {
		t.Errorf("HomePath = %q", got)
	}
	if got := JobRootPath("j1"); got != "/workspace/jobs/j1" {
		t.Errorf("JobRootPath = %q", got)
	}
}

// A tool's declared environment must reach the sandbox, and must beat the
// platform defaults for the same key (a tool that sets PATH means it — e.g. R
// living in a miniforge prefix). bwrap applies --setenv in order and the last
// one wins, so duplicates must not be emitted.
func TestBwrapArgvToolEnvOverridesDefaults(t *testing.T) {
	args := BwrapArgv(&Spec{}, BwrapOptions{
		Argv: []string{"true"},
		Env:  []string{"PATH=/pmo/miniforge3/bin:/usr/bin", "FOO=bar"},
	})

	setenv := map[string][]string{}
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--setenv" {
			setenv[args[i+1]] = append(setenv[args[i+1]], args[i+2])
		}
	}
	if got := setenv["PATH"]; len(got) != 1 || got[0] != "/pmo/miniforge3/bin:/usr/bin" {
		t.Fatalf("PATH setenv = %v, want exactly the tool's value", got)
	}
	if got := setenv["FOO"]; len(got) != 1 || got[0] != "bar" {
		t.Fatalf("FOO setenv = %v, want [bar]", got)
	}
	// The platform default is still there when the tool says nothing about it.
	if got := setenv["TMPDIR"]; len(got) != 1 || got[0] == "" {
		t.Fatalf("TMPDIR setenv = %v, want the platform default", got)
	}
}

// Apptainer/Singularity runs the image's filesystem, so only the declared
// mounts are bound (no systemRoDirs), and the shared exec interface is what
// makes one argv work on either binary.
func TestApptainerArgv(t *testing.T) {
	spec := &Spec{}
	spec.MustAdd(Mount{HostPath: "/host/ws", SandboxPath: "/workspace", Mode: ReadWrite, Origin: "builtin"})
	spec.MustAdd(Mount{HostPath: "/host/tool", SandboxPath: "/tool", Mode: ReadOnly, Origin: "tool"})

	args := ApptainerArgv(spec, ApptainerOptions{
		Image: "/shared/img/x.sif",
		Cwd:   "/workspace",
		Env:   []string{"PATH=/usr/bin", "FOO=bar"},
		Argv:  []string{"bash", "work.sh"},
	})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"exec", "--contain", "--no-home", "--cleanenv",
		"--pwd /workspace",
		"--bind /host/ws:/workspace",
		"--bind /host/tool:/tool:ro",
		"--env FOO=bar",
		"--env PATH=/usr/bin",
		"/shared/img/x.sif bash work.sh",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("apptainer argv missing %q:\n%s", want, joined)
		}
	}

	// One --env per key, so a tool's value cannot be shadowed by a duplicate.
	n := 0
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--env" && strings.HasPrefix(args[i+1], "PATH=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("PATH appears in %d --env flags, want 1", n)
	}

	// The image and the command come last, in order.
	if got := args[len(args)-3:]; got[0] != "/shared/img/x.sif" || got[1] != "bash" || got[2] != "work.sh" {
		t.Fatalf("tail = %v", got)
	}
}
