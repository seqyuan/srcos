// Package image pre-warms container images onto a filesystem the compute nodes
// can read.
//
// Why it exists: a `sandbox: apptainer` tool names a SIF with `image:`, and that
// file has to be readable on the compute node at job time. Pulling it there —
// on the critical path, potentially on many nodes at once, often without
// network — is slow and fragile. Pulling it once onto shared storage before any
// job needs it is the prewarm. SRCOS only wraps the runtime's own `pull` and
// records where the file landed; it does not invent an image registry.
package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seqyuan/srcos/internal/sandbox"
)

// Runner executes an external command. Tests replace it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner is the production Runner.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// PullOptions configures one pull.
type PullOptions struct {
	// Ref is the source, e.g. docker://ubuntu:22.04 or a local path.
	Ref string
	// Dest is the output path; it must end in .sif or .simg.
	Dest string
	// Bin is the container runtime; empty probes apptainer then singularity.
	Bin string
	// Force re-pulls even when Dest already exists.
	Force bool
}

// Pull materializes Ref at Dest and returns Dest.
//
// Without Force an existing file is accepted as already warm: a prewarm is
// meant to be idempotent, and re-downloading a multi-GB image on every
// invocation would defeat the point.
func Pull(ctx context.Context, r Runner, o PullOptions) (string, error) {
	if strings.TrimSpace(o.Ref) == "" {
		return "", errors.New("image ref is required")
	}
	if !strings.HasSuffix(o.Dest, ".sif") && !strings.HasSuffix(o.Dest, ".simg") {
		return "", fmt.Errorf("destination %q must end in .sif or .simg", o.Dest)
	}
	bin := o.Bin
	if bin == "" {
		p, ok, why := sandbox.ApptainerProbe()
		if !ok {
			return "", errors.New(why)
		}
		bin = p
	}
	if !o.Force {
		if _, err := os.Stat(o.Dest); err == nil {
			return o.Dest, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.Dest), 0o755); err != nil {
		return "", err
	}
	out, err := r.Run(ctx, bin, "pull", "--force", o.Dest, o.Ref)
	if err != nil {
		return "", fmt.Errorf("%s pull %s: %w: %s", bin, o.Ref, err, firstLine(out))
	}
	return o.Dest, nil
}

// List returns the container images in dir, sorted. A missing directory is an
// empty list, not an error: a site that has never prewarmed anything.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".sif") || strings.HasSuffix(name, ".simg") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// DefaultDest derives an output file name from a ref, inside dir.
//
//	docker://ubuntu:22.04  ->  <dir>/ubuntu-22.04.sif
func DefaultDest(dir, ref string) string {
	name := strings.TrimSpace(ref)
	if i := strings.Index(name, "://"); i >= 0 {
		name = name[i+3:]
	}
	name = strings.NewReplacer("/", "-", ":", "-", "@", "-").Replace(name)
	name = strings.Trim(name, "-")
	if name == "" {
		name = "image"
	}
	if !strings.HasSuffix(name, ".sif") && !strings.HasSuffix(name, ".simg") {
		name += ".sif"
	}
	return filepath.Join(dir, name)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
