package api

import (
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
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
	t, err := h.reader().VisibleManifest(username, body.Tool)
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
	if err := h.checkQuota(username, t, j); err != nil {
		writeJSON(w, 403, map[string]string{"error": err.Error()})
		return
	}

	id, dir, err := job.Submit(h.configDir(), username, t.ID, j)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 201, submitJobResponse{JobID: id, Dir: dir, Tool: t.ID})
}

// handleListJobs returns this user's instances.
//
// Scoped to the user on purpose: the instance list is a per-user view, and a
// leak here would expose another user's activity and endpoints. The
// management-side view (all users) belongs behind the admin role in Phase 3.
func (h *Handler) handleListJobs(w http.ResponseWriter, r *http.Request, username string) {
	if h.opts.ConfigDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no instance store is configured on this host"})
		return
	}
	views, err := h.reader().Instances(username,
		strings.TrimSpace(r.URL.Query().Get("tool")),
		strings.TrimSpace(r.URL.Query().Get("kind")))
	if err != nil {
		writeJSON(w, errorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"jobs": views})
}

// checkQuota enforces the user's aggregate ceiling for a tool.
//
// It runs *after* job.Validate, so a request is first checked against what the
// tool is willing to run with and then against what this user is allowed to
// consume. The two ceilings are independent: the tool's says "this is the most
// this analysis can use", the grant's says "this is the most you may take".
func (h *Handler) checkQuota(username string, t *tool.Tool, j *job.Job) error {
	if h.opts.Grants == nil {
		return nil
	}
	q := h.opts.Grants.QuotaFor(username, t.ID)
	if q.IsZero() {
		return nil
	}

	eff := job.EffectiveResources(j, t)
	want := grant.Usage{Instances: 1, CPU: eff.CPU}
	if eff.Memory != "" {
		if b, err := tool.ParseMemory(eff.Memory); err == nil {
			want.Memory = b
		}
	}

	used, err := h.usageFor(username, t.ID)
	if err != nil {
		return err
	}
	return grant.CheckQuota(q, used, want)
}

// usageFor sums a user's live instances of one tool.
//
// Terminal instances do not count: a finished job is not holding a slot, and
// charging for history would make the quota grow without bound.
func (h *Handler) usageFor(username, toolID string) (grant.Usage, error) {
	all, err := runtime.ListInstances(h.configDir())
	if err != nil {
		return grant.Usage{}, err
	}
	var u grant.Usage
	for _, inst := range all {
		if inst.User != username || inst.Tool != toolID || inst.State.Terminal() {
			continue
		}
		u.Instances++
		// The resources actually requested are not recorded on the instance, so
		// the tool's declaration is the conservative stand-in: it can only
		// over-count, which fails closed.
		u.CPU += inst.CPURequest()
		u.Memory += inst.MemoryRequestBytes()
	}
	return u, nil
}

// configDir returns the directory instances live under. It is derived from the
// user registry's config directory so the two never disagree.
func (h *Handler) configDir() string {
	if h.Registry == nil {
		return h.opts.ConfigDir
	}
	return config.DirOf(h.Registry)
}
