package sge

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/config"
)

// FromState translates the site's state.yaml section into a backend config.
// This is the only place the config schema and the backend meet: everything
// below works with Config, which is also what the tests build directly.
func FromState(s config.SGEState) (Config, error) {
	// The tunnel is the default: without it the endpoint names a compute node
	// and the route layer (which only accepts loopback) refuses it.
	tunnel := true
	if s.Tunnel != nil {
		tunnel = *s.Tunnel
	}
	renewBefore, err := parseOptionalDuration(s.RenewBefore)
	if err != nil {
		return Config{}, fmt.Errorf("sge.renew_before: %w", err)
	}
	renewFor, err := parseOptionalDuration(s.RenewFor)
	if err != nil {
		return Config{}, fmt.Errorf("sge.renew_for: %w", err)
	}
	warnBefore, err := parseOptionalDuration(s.WarnBefore)
	if err != nil {
		return Config{}, fmt.Errorf("sge.warn_before: %w", err)
	}
	cfg := Config{
		Qsub:          s.Qsub,
		Qstat:         s.Qstat,
		Qdel:          s.Qdel,
		Qalter:        s.Qalter,
		SubmitDir:     s.SubmitDir,
		RendezvousDir: s.RendezvousDir,
		SSH:           s.SSH,
		SSHUser:       s.SSHUser,
		SSHArgs:       s.SSHArgs,
		Tunnel:        tunnel,
		DirectDial:    s.DirectDial,
		Scheduler: SchedulerDefaults{
			DefaultQueue:   s.DefaultQueue,
			PE:             s.PE,
			PEAccounting:   s.PEAccounting,
			ThreadsPerCore: s.ThreadsPerCore,
			Project:        s.Project,
			RenewBefore:    renewBefore,
			RenewFor:       renewFor,
			WarnBefore:     warnBefore,
		},
		PollEvery: time.Duration(s.PollSeconds) * time.Second,
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseOptionalDuration reads a Go duration ("10m", "1h30m"). Empty is zero.
func parseOptionalDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (want Go duration like 10m or 1h30m)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", s)
	}
	return d, nil
}

// FromStateFile builds the backend when the site has enabled SGE in
// state.yaml. A disabled or absent section returns (nil, false, nil), which is
// what makes `backend: sge` on a host without a scheduler fail as "backend not
// registered" rather than run heavy work in the wrong place.
//
// The returned backend has no port allocator; the caller that owns the shared
// port pool must set Config.Ports before a service can open its tunnel.
func FromStateFile(configDir string) (*Backend, bool, error) {
	path := config.StatePath(configDir)
	// Do not create state.yaml just to look for a scheduler: a host (or a test)
	// without one must not gain a file as a side effect.
	if _, err := os.Stat(path); err != nil {
		return nil, false, nil
	}
	st, err := config.LoadState(path)
	if err != nil {
		return nil, false, err
	}
	if !st.SGE.Enabled {
		return nil, false, nil
	}
	cfg, err := FromState(st.SGE)
	if err != nil {
		return nil, false, err
	}
	return &Backend{Config: cfg}, true, nil
}
