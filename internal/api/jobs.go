package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/job"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// submitJobBody is the request shape for POST /api/jobs.
//
// It mirrors job.json but keeps the tool and the display name outside it: the
// tool is an addressing fact (it selects which interface validates the params)
// and the name is presentation, so neither belongs in the on-disk record where
// the sandbox could read them as instructions.
type submitJobBody struct {
	Tool      string            `json:"tool"`
	Name      string            `json:"name"`
	Params    map[string]any    `json:"params"`
	Resources *tool.Resources   `json:"resources"`
	Outputs   []string          `json:"outputs"`
	Tags      map[string]string `json:"tags"`
}

type submitJobResponse struct {
	JobID string `json:"jobId"`
	Dir   string `json:"dir"`
	Tool  string `json:"tool"`
}

// handleSubmitJob validates a submission against the tool's interface and drops
// it into the job directory.
//
// Validation happens here, not at run time, so a user gets the error while the
// form is still on screen. The same job.Validate is used by the CLI, so the two
// entry points cannot disagree about what a valid submission is.
func (h *Handler) handleSubmitJob(w http.ResponseWriter, r *http.Request, username string) {
	var body submitJobBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	if strings.TrimSpace(body.Tool) == "" {
		writeJSON(w, 400, map[string]string{"error": "tool is required"})
		return
	}
	t, err := h.findVisibleTool(username, body.Tool)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if t.Kind != tool.KindTask {
		writeJSON(w, 400, map[string]string{"error": "tool " + t.ID +
			" is a service; services are started, not submitted"})
		return
	}

	params := body.Params
	if params == nil {
		params = map[string]any{}
	}
	j := &job.Job{
		SchemaVersion: 1,
		Name:          strings.TrimSpace(body.Name),
		Params:        params,
		Tags:          body.Tags,
		Outputs:       body.Outputs,
		Resources:     body.Resources,
	}
	if j.Name == "" {
		j.Name = t.Name
	}
	if err := job.Validate(j, t); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	id, dir, err := job.Submit(h.configDir(), username, t.ID, j)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, submitJobResponse{JobID: id, Dir: dir, Tool: t.ID})
}

type jobView struct {
	ID        string            `json:"id"`
	Tool      string            `json:"tool"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	State     string            `json:"state"`
	ExitCode  int               `json:"exitCode"`
	Error     string            `json:"error,omitempty"`
	Endpoint  string            `json:"endpoint,omitempty"`
	RoutePath string            `json:"routePath,omitempty"`
	Backend   string            `json:"backend"`
	Sandbox   string            `json:"sandbox"`
	Limiter   string            `json:"limiter,omitempty"`
	Duration  string            `json:"duration,omitempty"`
	StartedAt string            `json:"startedAt,omitempty"`
	Outputs   []string          `json:"outputs,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// handleListJobs returns this user's instances.
//
// Scoped to the session user on purpose: the instance list is a per-user view,
// and a leak here would expose another user's activity and endpoints. The
// management-side view (all users) belongs behind the admin role in Phase 3.
func (h *Handler) handleListJobs(w http.ResponseWriter, r *http.Request, username string) {
	if h.opts.ConfigDir == "" {
		writeJSON(w, 503, map[string]string{"error": "no instance store is configured on this host"})
		return
	}
	all, err := runtime.ListInstances(h.opts.ConfigDir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	toolFilter := strings.TrimSpace(r.URL.Query().Get("tool"))
	kindFilter := strings.TrimSpace(r.URL.Query().Get("kind"))

	out := make([]jobView, 0, len(all))
	for _, i := range all {
		if i.User != username {
			continue
		}
		if toolFilter != "" && i.Tool != toolFilter {
			continue
		}
		if kindFilter != "" && i.Kind != kindFilter {
			continue
		}
		v := jobView{
			ID:        i.ID,
			Tool:      i.Tool,
			Kind:      i.Kind,
			Name:      i.JobName,
			State:     string(i.State),
			ExitCode:  i.ExitCode,
			Error:     i.Error,
			Endpoint:  i.Endpoint,
			RoutePath: i.RoutePath,
			Backend:   i.Backend,
			Sandbox:   i.Sandbox,
			Limiter:   i.Limiter,
			Duration:  i.Duration,
			Outputs:   i.Outputs,
			Tags:      i.Tags,
		}
		if !i.StartedAt.IsZero() {
			v.StartedAt = i.StartedAt.Format("2006-01-02T15:04:05Z07:00")
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"jobs": out})
}

// configDir returns the directory instances live under. It is derived from the
// user registry's config directory so the two never disagree.
func (h *Handler) configDir() string {
	if h.Registry == nil {
		return h.opts.ConfigDir
	}
	return config.DirOf(h.Registry)
}
