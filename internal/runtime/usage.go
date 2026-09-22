package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// UnitUsage is a snapshot of what a unit is using right now.
//
// It is a *snapshot*, not a time series: the admin page asks for it when it
// renders, and SRCOS keeps no history. That is deliberate — a metric store is a
// database by another name (AGENTS.md: 不引入外部数据库), and "is this instance
// eating the box?" is answerable from one reading.
type UnitUsage struct {
	CPUSeconds float64
	RSSBytes   uint64
	SampledAt  time.Time
}

// UnitSampler is implemented by backends that can report resource usage for a
// running unit. It is a backends' business because only the backend knows where
// the numbers live: a systemd unit has cgroup counters, a plain child process
// only has /proc, and an SGE job would have to ask the scheduler.
type UnitSampler interface {
	UnitUsage(ctx context.Context, inst *Instance) (UnitUsage, bool)
}

// userHZ is the kernel's clock tick rate for /proc accounting (utime/stime are
// in ticks). Linux has used 100 on every architecture that runs SRCOS for
// decades; the cost of being wrong is a 10× error in a display-only number,
// which is why it is a named constant rather than a syscall.
const userHZ = 100

// UnitUsage implements runtime.UnitSampler.
//
// With a user systemd the cgroup counters are authoritative and cover the whole
// unit (including children the tool spawned). Without one, the pid SRCOS
// recorded is the only handle, and the numbers are that process alone — which
// under-reports a tool that fans out, and says so via the second return value
// only in the sense that it is "known, but partial". The display is a hint for
// an operator, not a billing figure.
func (l *Local) UnitUsage(ctx context.Context, inst *Instance) (UnitUsage, bool) {
	if l.useSystemd() && inst.BackendRef != "" {
		if u, ok := systemdUnitUsage(ctx, inst.BackendRef); ok {
			return u, true
		}
	}
	if inst.PID <= 0 {
		return UnitUsage{}, false
	}
	// The identity check matters here too: a reused pid's counters belong to
	// someone else, and reporting them as this instance's would be a lie.
	if inst.PIDStart == 0 {
		return UnitUsage{}, false
	}
	start, ok := pidStartTime(inst.PID)
	if !ok || start != inst.PIDStart {
		return UnitUsage{}, false
	}
	return procUsage(inst.PID)
}

// systemdUnitUsage reads the cgroup counters of a transient unit.
func systemdUnitUsage(ctx context.Context, unit string) (UnitUsage, bool) {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", unit,
		"-p", "CPUUsageNSec", "-p", "MemoryCurrent", "--value").Output()
	if err != nil {
		return UnitUsage{}, false
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return UnitUsage{}, false
	}
	ns, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return UnitUsage{}, false
	}
	rss, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		// MemoryCurrent prints "[not set]" or UINT64_MAX for "unlimited"; the
		// CPU figure is still worth reporting.
		return UnitUsage{CPUSeconds: float64(ns) / 1e9, SampledAt: time.Now()}, true
	}
	return UnitUsage{
		CPUSeconds: float64(ns) / 1e9,
		RSSBytes:   rss,
		SampledAt:  time.Now(),
	}, true
}

// procUsage reads one process's own accounting from /proc.
func procUsage(pid int) (UnitUsage, bool) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return UnitUsage{}, false
	}
	// Parse from the last ')': the command name may contain spaces and
	// parentheses. After it come state, ppid, … with utime at field 14 and
	// stime at 15 → indices 11 and 12 of what remains.
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return UnitUsage{}, false
	}
	fields := strings.Fields(string(stat[i+1:]))
	if len(fields) < 13 {
		return UnitUsage{}, false
	}
	utime, err1 := strconv.ParseUint(fields[11], 10, 64)
	stime, err2 := strconv.ParseUint(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return UnitUsage{}, false
	}

	var rss uint64
	if status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			rest, ok := strings.CutPrefix(line, "VmRSS:")
			if !ok {
				continue
			}
			f := strings.Fields(rest)
			if len(f) < 1 {
				continue
			}
			if kb, err := strconv.ParseUint(f[0], 10, 64); err == nil {
				rss = kb * 1024
			}
			break
		}
	}
	return UnitUsage{
		CPUSeconds: float64(utime+stime) / userHZ,
		RSSBytes:   rss,
		SampledAt:  time.Now(),
	}, true
}

// UsageFor samples a unit through its backend, if that backend can.
//
// The orchestration layer exposes it as a method so callers (the admin surface)
// do not have to know which backend a tool declared.
func (r *Runner) UsageFor(ctx context.Context, inst *Instance) (UnitUsage, bool) {
	b, ok := r.opts.Backends[inst.Backend]
	if !ok {
		return UnitUsage{}, false
	}
	sampler, ok := b.(UnitSampler)
	if !ok {
		return UnitUsage{}, false
	}
	return sampler.UnitUsage(ctx, inst)
}

// RequestedResources is the declared ceiling this instance is allowed to use,
// for the admin view that shows usage next to it.
func (i *Instance) RequestedResources() (cpu int, memory string) {
	if i == nil {
		return 0, ""
	}
	return i.RequestedCPU, i.RequestedMemory
}

// FormatBytes renders a byte count the way a human reads it. It lives here
// (rather than in the web layer) so the number is formatted once, next to the
// code that samples it.
func FormatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(b)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}
