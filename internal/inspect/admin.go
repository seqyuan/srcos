package inspect

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the operator's view: every user's instances, and the
// authorization state of every tool. It is the read half of the management
// surface, and it lives here for the usual reason — the admin API and the admin
// page must not have two opinions about what an instance is or who can use a
// tool.

// UsageSampler reports what a unit is using right now. It is satisfiable by
// *runtime.Runner, and an interface so a deployment without a supervisor (or a
// test) can simply not sample.
type UsageSampler interface {
	UsageFor(ctx context.Context, inst *runtime.Instance) (runtime.UnitUsage, bool)
}

// AdminUsage is one resource snapshot, formatted for display once.
type AdminUsage struct {
	CPUSeconds float64 `json:"cpuSeconds"`
	RSSBytes   uint64  `json:"rssBytes"`
	RSSHuman   string  `json:"rssHuman"`
	SampledAt  string  `json:"sampledAt"`
}

// AdminInstance is one instance as an operator sees it: whose it is, what it
// asked for, and (when the backend can say) what it is using.
type AdminInstance struct {
	Owner string       `json:"owner"`
	Inst  InstanceView `json:"instance"`
	Usage *AdminUsage  `json:"usage,omitempty"`
	Tool  string       `json:"tool"`
}

// AdminInstances lists every user's instances, newest first (the instance
// records are already ordered that way).
func (r *Reader) AdminInstances(ctx context.Context, sampler UsageSampler) ([]AdminInstance, error) {
	if r.ConfigDir == "" {
		return nil, fmt.Errorf("%w: no instance store is configured", ErrUnavailable)
	}
	all, err := runtime.ListInstances(r.ConfigDir)
	if err != nil {
		return nil, err
	}
	out := make([]AdminInstance, 0, len(all))
	for _, inst := range all {
		view := AdminInstance{Owner: inst.User, Inst: ViewOfInstance(inst), Tool: inst.Tool}
		// Sampling costs a process spawn (systemctl) or a /proc read; it is
		// only worth doing for something that claims to be alive.
		if sampler != nil && !inst.State.Terminal() {
			if usage, ok := sampler.UsageFor(ctx, inst); ok {
				view.Usage = &AdminUsage{
					CPUSeconds: usage.CPUSeconds,
					RSSBytes:   usage.RSSBytes,
					RSSHuman:   runtime.FormatBytes(usage.RSSBytes),
					SampledAt:  usage.SampledAt.Format("2006-01-02T15:04:05Z07:00"),
				}
			}
		}
		out = append(out, view)
	}
	return out, nil
}

// AdminInstance is a lookup by exact id across all users.
func (r *Reader) AdminInstance(id string) (*runtime.Instance, error) {
	if r.ConfigDir == "" {
		return nil, fmt.Errorf("%w: no instance store is configured", ErrUnavailable)
	}
	inst, err := runtime.LoadInstance(runtime.InstancePath(r.ConfigDir, id))
	if err != nil {
		return nil, fmt.Errorf("%w: no instance %s", ErrNotFound, id)
	}
	return inst, nil
}

// AdminLogs returns the tail of any instance's log, whatever user owns it.
func (r *Reader) AdminLogs(id string, tail int) (string, error) {
	inst, err := r.AdminInstance(id)
	if err != nil {
		return "", err
	}
	if tail <= 0 {
		tail = DefaultLogTail
	}
	if tail > MaxLogTail {
		tail = MaxLogTail
	}
	data, err := os.ReadFile(inst.LogPath)
	if err != nil {
		return "", fmt.Errorf("%w: no log for %s", ErrNotFound, id)
	}
	return TailLines(string(data), tail), nil
}

// ToolSharingPolicy is the part of the authorization policy this view needs.
// It is an interface so the view can be rendered from a snapshot (the admin
// page) or from the live policy (the API) without either owning the other.
type ToolSharingPolicy interface {
	Grant(tool string) (grant.Grant, bool)
	ReachesAnyone(tool string) bool
	GroupNames() []string
}

// AdminTool is one tool as an operator sees it: the contract, plus the
// authorization state that decides who can reach it.
type AdminTool struct {
	ToolView
	// Reachable is false when only admins can use it — which is the default,
	// and therefore the most useful thing to show in a table.
	Reachable bool         `json:"reachable"`
	Grant     *grant.Grant `json:"grant,omitempty"`
	// Note explains the state in one line ("未授权：非管理员不可见").
	Note string `json:"note,omitempty"`
}

// AdminTools lists every tool package on this host with its authorization
// state.
//
// Unlike Tools it is *not* filtered by grants: the operator's job is to see
// what exists, including what nobody can use yet.
func (r *Reader) AdminTools(policy ToolSharingPolicy) ([]AdminTool, error) {
	if r.ToolsDir == "" {
		return nil, fmt.Errorf("%w: no tool directory is configured", ErrUnavailable)
	}
	manifests, err := tool.Discover(r.ToolsDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].ID < manifests[j].ID })

	out := make([]AdminTool, 0, len(manifests))
	for _, t := range manifests {
		v := AdminTool{
			ToolView:  viewOf(t, r.storageViewsFor(t)),
			Reachable: policy == nil || policy.ReachesAnyone(t.ID),
		}
		if policy != nil {
			if g, ok := policy.Grant(t.ID); ok {
				v.Grant = &g
			}
		}
		switch {
		case v.Grant == nil:
			v.Note = "未授权：非管理员不可见"
		case len(v.Grant.Users) == 0 && len(v.Grant.Groups) == 0 && !v.Grant.Public:
			v.Note = "授权给了没有人（等于不可见）"
		}
		out = append(out, v)
	}
	return out, nil
}

// ViewOfInstance renders a runtime record the way every instance answer does.
//
// It is exported for the management surface, which holds runtime records (it
// stops them, so it loads them) and must present them identically to
// /api/jobs and the MCP tools.
func ViewOfInstance(i *runtime.Instance) InstanceView { return viewFromInstance(i) }

// TailLines returns the last n lines, with a marker when earlier ones were
// dropped. Exported for the admin log view, which reads another user's log.
func TailLines(s string, n int) string { return tailLines(s, n) }

// LogLabel is the human label of an instance's log, for a UI that links to it.
func LogLabel(inst *runtime.Instance) string {
	if inst == nil {
		return ""
	}
	parts := strings.Split(inst.LogPath, "/")
	if len(parts) == 0 {
		return inst.LogPath
	}
	return parts[len(parts)-1]
}
