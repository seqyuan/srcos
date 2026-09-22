package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// BwrapOptions is everything the materializer needs beyond the mount table.
type BwrapOptions struct {
	// Cwd is the working directory (--chdir). It must be inside a rw mount.
	Cwd string
	// Env is the complete environment for the sandboxed process. The host
	// environment is NOT inherited: SRCOS's own env may hold secrets, and a
	// tool must not depend on ambient variables it did not declare.
	Env []string
	// Argv is the command to execute inside the sandbox.
	Argv []string
}

// systemRoDirs are the host directories a Linux userspace needs to execute
// anything at all. They contain no user data.
var systemRoDirs = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
}

// systemRoFiles are the individual /etc entries that dynamic linking, name
// resolution and user lookup need. Binding all of /etc would leak host
// configuration (and any credentials a site keeps there), so only these are
// exposed.
var systemRoFiles = []string{
	"/etc/ld.so.cache",
	"/etc/ld.so.conf",
	"/etc/nsswitch.conf",
	"/etc/resolv.conf",
	"/etc/hosts",
	"/etc/passwd",
	"/etc/group",
	"/etc/localtime",
	"/etc/ssl/certs",
	"/etc/alternatives",
	"/etc/terminfo",
}

// DefaultCwd returns the working directory to use when the caller does not
// specify one.
const DefaultCwd = PathWorkspace

// PathToolBin is the conventional sandbox directory for tool-provided
// binaries. It is first on the sandbox PATH.
//
// Why a dedicated directory instead of /usr/local/bin: bubblewrap cannot
// create a mount point inside an already-bound path, and every system
// directory below is bound read-only. Mounting to /usr/local/bin/foo would fail
// with "Read-only file system". /opt/srcos/bin is outside all of them, so
// bubblewrap creates it on demand.
const PathToolBin = "/opt/srcos/bin"

// DefaultPath is the PATH installed in a sandbox. It is explicit because the
// host environment is cleared.
const DefaultPath = PathToolBin +
	":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// BwrapArgv builds the full `bwrap ... <argv>` argument vector.
//
// Order matters: the namespace and builtin mounts come first, then the
// declared mounts, then the environment, then the command. `--clearenv`
// guarantees nothing from the host leaks in.
func BwrapArgv(spec *Spec, o BwrapOptions) []string {
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--proc", "/proc",
		"--dev", "/dev",
		// A fresh tmpfs for /tmp: scratch space that never touches the host
		// and disappears with the instance.
		"--tmpfs", PathTmp,
	}

	// Host userspace, read-only. We deliberately do NOT `--ro-bind / /`:
	// that would expose the OS user's real home and every group-readable
	// directory on the host.
	for _, d := range systemRoDirs {
		if _, err := os.Stat(d); err == nil {
			args = append(args, "--ro-bind", d, d)
		}
	}
	for _, f := range systemRoFiles {
		if _, err := os.Stat(f); err == nil {
			args = append(args, "--ro-bind", f, f)
		}
	}

	for _, m := range spec.Mounts() {
		flag := "--ro-bind"
		if m.Mode == ReadWrite {
			flag = "--bind"
		}
		args = append(args, flag, m.HostPath, m.SandboxPath)
	}

	args = append(args, "--clearenv")
	env := append([]string{"PATH=" + DefaultPath, "TMPDIR=" + PathTmp}, o.Env...)
	sort.Strings(env) // deterministic argv, easier to diff in audit logs
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		// bwrap's signature is `--setenv VAR VALUE` (two arguments), not
		// `--setenv VAR=VALUE`.
		args = append(args, "--setenv", k, v)
	}

	cwd := o.Cwd
	if cwd == "" {
		cwd = DefaultCwd
	}
	args = append(args, "--chdir", cwd, "--")
	args = append(args, o.Argv...)
	return args
}

// BwrapAvailable reports whether bubblewrap can actually start a sandbox on
// this host. A present binary is not enough: Ubuntu 24.04+ ships an AppArmor
// restriction that makes bwrap fail with "setting up uid map: Permission
// denied" unless the binary has been granted the `userns` permission. That can
// only be learned by trying.
//
// The result is cached because the probe costs a process spawn.
var bwrapProbe struct {
	done bool
	ok   bool
	path string
	why  string
}

// BwrapProbe returns the cached probe outcome.
func BwrapProbe() (path string, ok bool, why string) {
	if bwrapProbe.done {
		return bwrapProbe.path, bwrapProbe.ok, bwrapProbe.why
	}
	bwrapProbe.done = true

	p, err := exec.LookPath("bwrap")
	if err != nil {
		bwrapProbe.why = "bwrap not found on PATH (install bubblewrap, or use sandbox: none)"
		return "", false, bwrapProbe.why
	}
	bwrapProbe.path = p

	// The probe is the cheapest possible sandbox: builtin system dirs only,
	// cwd at the namespace root, run /bin/true. If this fails, nothing works.
	spec := &Spec{}
	argv := BwrapArgv(spec, BwrapOptions{Argv: []string{"/bin/true"}, Cwd: "/"})

	cmd := exec.Command(p, argv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		bwrapProbe.why = "bwrap cannot start a sandbox: " + msg
		if strings.Contains(string(out), "uid map") {
			bwrapProbe.why += "\n  hint: unprivileged user namespaces are blocked. On Ubuntu 24.04+ grant\n" +
				"  `userns` to bwrap via an AppArmor profile instead of disabling the sysctl globally:\n" +
				"    sudo tee /etc/apparmor.d/bwrap >/dev/null <<'P'\n" +
				"    abi <abi/4.0>,\n" +
				"    include <tunables/global>\n\n" +
				"    profile bwrap /usr/bin/bwrap flags=(unconfined) {\n" +
				"      userns,\n" +
				"      include if exists <local/bwrap>\n" +
				"    }\n" +
				"    P\n" +
				"    sudo apparmor_parser -r /etc/apparmor.d/bwrap"
		}
		return p, false, bwrapProbe.why
	}
	bwrapProbe.ok = true
	return p, true, ""
}

// SandboxPathIsReserved reports whether a sandbox path sits inside one of the
// read-only system directories that BwrapArgv binds.
//
// Use it to reject mounts that must fail: bubblewrap creates missing mount
// points on demand, but it cannot create one inside an already-bound path, so
// such a mount dies with "Can't create file at ...: Read-only file system" —
// a confusing error that is really a manifest mistake.
func SandboxPathIsReserved(p string) bool {
	p = cleanSlash(p)
	for _, d := range systemRoDirs {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	for _, f := range systemRoFiles {
		if p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// FirstLine returns the first line of s, trimmed. Exposed for error rendering.
func FirstLine(s string) string { return firstLine(s) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// HostDirForTool is a convenience for callers that need the tool package path.
func HostDirForTool(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}
