// Package environment is the *named environment* provider: where an interpreter
// and its libraries live (design: docs/plans/2026-09-26-unified-service-instantiation-design.md §2.1).
//
// It exists for the same reason the storage provider does (ADR-020): the host
// fact belongs to the administrator, not to every tool package. A tool says
// `environment: r-miniforge`; the deployment says what that means *here*.
// Otherwise every tool embeds `/Volumes/data/pmo/miniforge3` and stops being
// portable — the tool package would be a copy of this machine.
//
// Two rules are load-bearing:
//
//   - **The root is mounted at the same path it has on the host.** Interpreters
//     hardcode absolute paths (measured: R's launcher refers to
//     `/Volumes/data/…/bin/sed`, so mounting the prefix anywhere else makes it
//     fail with a confusing "not found").
//   - **It is read-only.** This is *environment*; data goes through storages
//     (AGENTS.md: 环境与数据要分开).
//
// A unit's final environment is ordered platform → environment → the tool's own
// `env:`, so a tool can override an environment, and an environment can override
// the platform defaults.
package environment

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/seqyuan/srcos/internal/sandbox"
)

// Environment is one declared interpreter/dependency tree.
type Environment struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`
	// Root is the host prefix, mounted read-only at the same sandbox path.
	Root string `yaml:"root" json:"root"`
	// Env is the environment units using this get. A PATH entry, if present,
	// must include the platform's own bin directory (see PathRequirementError).
	Env []string `yaml:"env" json:"env"`
	// Provides are executables that must exist under Root/bin. They turn "R is
	// installed but has no shiny" from a runtime surprise into a registration
	// warning (measured: the R on PATH was not the one with the packages).
	Provides []string `yaml:"provides,omitempty" json:"provides,omitempty"`
}

// ConfigFile mirrors config/environments.yaml.
type ConfigFile struct {
	Environments []Environment `yaml:"environments"`
}

// Provider answers "what is this environment, here".
type Provider interface {
	List() []Environment
	Get(id string) (Environment, bool)
}

// ReachabilityChecker is implemented by a provider that can look at the host.
type ReachabilityChecker interface {
	// CheckReachable reports problems a human has to fix, one line each.
	CheckReachable() []string
}

// idRe allows dots: environment ids are naturally versionish (`py-3.11`).
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// OriginFor is the mount origin recorded for an environment's root.
func OriginFor(id string) string { return "environment:" + id }

// Load reads config/environments.yaml. A missing file is not an error: it means
// this deployment declares no named environments, which is valid (tools that
// reference one then fail loudly).
func Load(path string) (Provider, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return New(nil)
		}
		return nil, err
	}
	var f ConfigFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return New(f.Environments)
}

// New validates and builds a provider. Every rule here is a declaration-level
// rule: it can be checked without the host, so `tool validate` works anywhere.
func New(items []Environment) (Provider, error) {
	seen := map[string]bool{}
	for i := range items {
		e := &items[i]
		if !idRe.MatchString(e.ID) {
			return nil, fmt.Errorf("environment[%d]: id %q must match %s", i, e.ID, idRe)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("environment %s is declared twice", e.ID)
		}
		seen[e.ID] = true
		if !filepath.IsAbs(e.Root) {
			return nil, fmt.Errorf("environment %s: root must be an absolute host path, got %q", e.ID, e.Root)
		}
		if err := validateEnv(e); err != nil {
			return nil, err
		}
	}
	return &provider{items: items}, nil
}

func validateEnv(e *Environment) error {
	for i, kv := range e.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("environment %s: env[%d] must be VAR=VALUE, got %q", e.ID, i, kv)
		}
		if k == "PATH" && !hasPathEntry(v, sandbox.PathToolBin) {
			return fmt.Errorf("environment %s: PATH must include %s — it is the first entry of the "+
				"platform default and the only way a tool finds the binaries SRCOS mounts for it",
				e.ID, sandbox.PathToolBin)
		}
	}
	for i, name := range e.Provides {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/\\") {
			return fmt.Errorf("environment %s: provides[%d] must be a bare executable name, got %q", e.ID, i, name)
		}
	}
	return nil
}

func hasPathEntry(pathValue, want string) bool {
	for _, p := range strings.Split(pathValue, ":") {
		if p == want {
			return true
		}
	}
	return false
}

// PathRequirementError documents the one rule an environment author trips over.
var PathRequirementError = errors.New("PATH must include " + sandbox.PathToolBin)

type provider struct {
	items []Environment
}

func (p *provider) List() []Environment {
	out := append([]Environment(nil), p.items...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *provider) Get(id string) (Environment, bool) {
	for _, e := range p.items {
		if e.ID == id {
			return e, true
		}
	}
	return Environment{}, false
}

// CheckOne looks at the host for a single environment: does its root exist, are
// the declared executables there. Exported so `tool validate` can report the
// one an author actually cares about.
func CheckOne(e Environment) []string {
	var problems []string
	if _, err := os.Stat(e.Root); err != nil {
		return []string{fmt.Sprintf(
			"environment %s: root %s is not readable by the SRCOS OS user (%v)", e.ID, e.Root, err)}
	}
	for _, name := range e.Provides {
		bin := filepath.Join(e.Root, "bin", name)
		info, err := os.Stat(bin)
		if err != nil {
			problems = append(problems, fmt.Sprintf("environment %s: %s is missing", e.ID, bin))
			continue
		}
		if info.Mode()&0o111 == 0 {
			problems = append(problems, fmt.Sprintf("environment %s: %s is not executable", e.ID, bin))
		}
	}
	return problems
}

// CheckReachable looks at the host for every declared environment.
func (p *provider) CheckReachable() []string {
	var problems []string
	for _, e := range p.List() {
		problems = append(problems, CheckOne(e)...)
	}
	return problems
}

// MountsFor adds each environment's root to a mount table, read-only and at its
// own host path.
//
// The read side (package inspect) deliberately does not call this: environment
// mounts are identical on both sides of the boundary and are never where a
// user's artifacts live, so resolving *into* them is not a thing the viewer or
// `read_file` needs to do.
func MountsFor(spec *sandbox.Spec, envs []Environment) error {
	for _, e := range envs {
		if err := spec.Add(sandbox.Mount{
			HostPath:    e.Root,
			SandboxPath: e.Root,
			Mode:        sandbox.ReadOnly,
			Origin:      OriginFor(e.ID),
		}); err != nil {
			return fmt.Errorf("environment %s: %w", e.ID, err)
		}
	}
	return nil
}
