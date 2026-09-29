package sandbox

import (
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ContainerPlaceholder is the argv element an off-host builder puts where the
// container runtime binary belongs.
//
// The login node that builds a job's argv may not be the node that runs it, and
// on a cluster only the compute nodes have apptainer/singularity installed. The
// SGE job script therefore resolves this placeholder to `apptainer` (falling
// back to `singularity`) at execution time.
const ContainerPlaceholder = "__SRCOS_CONTAINER__"

// ApptainerOptions is everything the apptainer materializer needs beyond the
// mount table.
type ApptainerOptions struct {
	// Image is the container image (a .sif on the shared filesystem). The tool
	// contract requires it when sandbox: apptainer.
	Image string
	// Cwd is --pwd. It must be inside a mount that exists in the container.
	Cwd string
	// Env is the complete environment. --cleanenv guarantees nothing from the
	// host leaks in; each entry is applied with --env.
	Env []string
	// Argv is the command to run inside the container.
	Argv []string
}

// ApptainerArgv builds the full `apptainer exec ... <image> <argv>` vector.
//
// It deliberately uses flags common to Apptainer and Singularity CE (the
// cluster this was written against ships Singularity CE 4, not Apptainer; the
// two share the exec interface), so one argv works on either binary:
//
//	--contain    minimal /dev and empty /home, /tmp instead of the host's
//	--cleanenv   do not inherit the host environment
//	--no-home    do not bind the real home (the virtual home is bound below)
//	--pwd        working directory inside the container
//	--bind       one host path per declared mount, ro unless the mount is rw
//	--env        one variable per declared environment entry
//
// Unlike bubblewrap, the container filesystem comes from the image, so only the
// declared mounts are bound; there is no systemRoDirs list to add.
func ApptainerArgv(spec *Spec, o ApptainerOptions) []string {
	cwd := o.Cwd
	if cwd == "" {
		cwd = DefaultCwd
	}
	args := []string{
		"exec",
		"--contain",
		"--no-home",
		"--cleanenv",
		"--pwd", cwd,
	}

	for _, m := range spec.Mounts() {
		// The destination is created by apptainer if it does not exist in the
		// image, which is what lets /workspace, /tool and /flow be mounted into
		// an arbitrary base image.
		bind := m.HostPath + ":" + m.SandboxPath
		if m.Mode == ReadOnly {
			bind += ":ro"
		}
		args = append(args, "--bind", bind)
	}

	// Environment, sorted for a deterministic (diffable) command line. A tool's
	// own entry wins over a platform default because the last --env for a name
	// is the one container sees; building a map first makes that explicit.
	envMap := map[string]string{}
	var keys []string
	for _, kv := range o.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if _, seen := envMap[k]; !seen {
			keys = append(keys, k)
		}
		envMap[k] = v
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+envMap[k])
	}

	args = append(args, o.Image)
	args = append(args, o.Argv...)
	return args
}

// probeResult caches one container-runtime probe for the process lifetime.
type probeResult struct {
	once sync.Once
	path string
	ok   bool
	why  string
}

var apptainerProbe probeResult

// apptainerProbeFn is the probe ApptainerProbe runs. It is a variable so a test
// can substitute one: the real answer is a property of the host, and a machine
// that happens to have a same-named binary installed must not decide whether a
// unit test passes.
var apptainerProbeFn = probeApptainer

// ApptainerProbe returns the container runtime to use, preferring Apptainer and
// falling back to Singularity CE (same exec interface). A present binary is not
// enough — the probe runs `<bin> --version` and checks that the output *looks
// like* a container runtime, not merely that the command exited 0.
//
// Checking the output is what a same-named binary makes necessary: Ubuntu ships
// the pygame game "Singularity" as /usr/games/singularity, and it exits 0 on
// `--version` while printing "Singularity 1.00 (commit: ...)". Trusting the exit
// code alone accepted it, so every `sandbox: apptainer` tool then failed later,
// inside the job, with the failure blamed on the tool rather than the runtime.
func ApptainerProbe() (path string, ok bool, why string) {
	apptainerProbe.once.Do(func() {
		apptainerProbe.path, apptainerProbe.ok, apptainerProbe.why = apptainerProbeFn()
	})
	return apptainerProbe.path, apptainerProbe.ok, apptainerProbe.why
}

// SetApptainerProbeForTest replaces the probe and clears the cached result,
// returning a function that restores both. It is exported because the callers
// that need a deterministic probe (the sandbox tests in internal/runtime) live
// in another package.
func SetApptainerProbeForTest(fn func() (string, bool, string)) func() {
	prev := apptainerProbeFn
	apptainerProbeFn = fn
	resetApptainerProbe()
	return func() {
		apptainerProbeFn = prev
		resetApptainerProbe()
	}
}

func resetApptainerProbe() { apptainerProbe = probeResult{} }

// versionBanner matches what a real container runtime prints on `--version`:
//
//	apptainer version 1.3.3
//	singularity version 4.1.0
//
// The word "version" after the name is what distinguishes it from Ubuntu's
// pygame game, which prints "Singularity 1.00". Matching anywhere in the output
// (rather than anchoring to the first line) tolerates a wrapper that prints a
// banner before the real version line.
var versionBanner = regexp.MustCompile(`(?i)\b(apptainer|singularity)\s+version\s+\S`)

func probeApptainer() (path string, ok bool, why string) {
	p, err := exec.LookPath("apptainer")
	if err != nil {
		if sp, serr := exec.LookPath("singularity"); serr == nil {
			p = sp
		} else {
			return "", false, "neither apptainer nor singularity is on PATH (install one, or use sandbox: none|bwrap)"
		}
	}
	out, err := exec.Command(p, "--version").CombinedOutput()
	if err != nil {
		msg := firstLine(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return p, false, p + " cannot even report its version: " + msg
	}
	if !versionBanner.Match(out) {
		msg := firstLine(string(out))
		if msg == "" {
			msg = "no output"
		}
		return p, false, p + " is on PATH but does not look like apptainer/singularity (--version said: " + msg + ")"
	}
	return p, true, ""
}
