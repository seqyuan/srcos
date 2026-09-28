package sandbox

import (
	"os/exec"
	"sort"
	"strings"
	"sync"
)

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

// apptainerProbe is the cached result of looking for a usable container runtime.
var apptainerProbe struct {
	once sync.Once
	path string
	ok   bool
	why  string
}

// ApptainerProbe returns the container runtime to use, preferring Apptainer and
// falling back to Singularity CE (same exec interface). A present binary is not
// enough — the probe runs `<bin> --version`, because a binary that cannot even
// report its version (a broken install, a user that NSS cannot resolve) must be
// reported with a reason rather than failing later inside a job.
func ApptainerProbe() (path string, ok bool, why string) {
	apptainerProbe.once.Do(func() {
		apptainerProbe.path, apptainerProbe.ok, apptainerProbe.why = probeApptainer()
	})
	return apptainerProbe.path, apptainerProbe.ok, apptainerProbe.why
}

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
	return p, true, ""
}
