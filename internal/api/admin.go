package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the management surface: the things an operator does, rather than
// the things a user does. It is the API half of the admin page, and it is a
// front-end over exactly the operations the CLI performs — `grant set`,
// `svc stop` — so the two cannot drift into different meanings of "granted".
//
// Everything here is admin-only, and "admin" is the *policy's* answer
// (grant.Policy.IsAdmin), not a flag carried by the request.
//
// Writes mutate the live policy in place and then persist it, in that order:
// the change takes effect on this gateway immediately (which is what an
// operator expects after clicking 保存), and a failed save is reported instead
// of being swallowed — "the change was not saved" is something the operator has
// to know before relying on it.

// adminHandler serves /api/admin/*. It returns true when it claimed the request.
func (h *Handler) adminHandler(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin")
	if !strings.HasPrefix(path, "/") {
		return false
	}
	if h.opts.Policy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the management surface is not configured on this host",
		})
		return true
	}

	username := h.requireAdmin(w, r)
	if username == "" {
		return true
	}

	// Flows have their own file (the management surface's write half is the
	// canvas): /api/admin/flows...
	if strings.HasPrefix(path, "/flows") {
		h.adminFlows(w, r)
		return true
	}

	switch {
	case path == "/instances" && r.Method == http.MethodGet:
		instances, err := h.adminInstances(r)
		if err != nil {
			writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
			return true
		}
		writeJSON(w, 200, map[string]any{"instances": instances})

	case strings.HasPrefix(path, "/instances/") && strings.HasSuffix(path, "/stop") && r.Method == http.MethodPost:
		h.adminStopInstance(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/instances/"), "/stop"))

	case strings.HasPrefix(path, "/instances/") && strings.HasSuffix(path, "/logs") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/instances/"), "/logs")
		tail := 0
		if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				tail = n
			}
		}
		log, err := h.reader().AdminLogs(id, tail)
		if err != nil {
			writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
			return true
		}
		writeJSON(w, 200, map[string]any{"instance": id, "log": log})

	case path == "/tools" && r.Method == http.MethodGet:
		tools, err := h.adminTools()
		if err != nil {
			writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
			return true
		}
		writeJSON(w, 200, map[string]any{"tools": tools})

	case strings.HasPrefix(path, "/grants/") && r.Method == http.MethodPut:
		h.adminSetGrant(w, r, username, strings.TrimPrefix(path, "/grants/"))

	case strings.HasPrefix(path, "/grants/") && r.Method == http.MethodDelete:
		h.adminRemoveGrant(w, r, username, strings.TrimPrefix(path, "/grants/"))

	case path == "/groups" && r.Method == http.MethodGet:
		policy := h.opts.Policy.Snapshot()
		writeJSON(w, 200, map[string]any{"groups": policy.Groups, "admins": policy.Admins})

	case path == "/groups" && r.Method == http.MethodPut:
		h.adminSetGroup(w, r, username)

	case path == "/admins" && r.Method == http.MethodPut:
		h.adminSetAdmins(w, r, username)

	case path == "/policy" && r.Method == http.MethodGet:
		policy := h.opts.Policy.Snapshot()
		writeJSON(w, 200, map[string]any{
			"admins":        policy.Admins,
			"groups":        policy.Groups,
			"default_allow": policy.DefaultAllow,
			"grants":        policy.Grants,
		})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
	return true
}

// requireAdmin resolves the identity and refuses anyone the policy does not
// call an admin.
func (h *Handler) requireAdmin(w http.ResponseWriter, r *http.Request) string {
	ident, ok := h.identity(w, r)
	if !ok {
		return ""
	}
	if !h.opts.Policy.IsAdmin(ident.User) {
		// 403, not 404: an authenticated user is allowed to know that a
		// management surface exists and that it is not theirs.
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this is an administrator-only surface",
		})
		return ""
	}
	return ident.User
}

// ─────────────────────────────────────────────────────────────────────────
// 实例
// ─────────────────────────────────────────────────────────────────────────

// adminInstances is the data behind the instance table (and behind the page,
// which calls this same method).
//
// The filters are the query string's business, so they are applied here rather
// than pushed into the read model: `inspect` answers "what instances are there",
// and the API decides which slice of that answer a caller asked for.
func (h *Handler) adminInstances(r *http.Request) ([]inspect.AdminInstance, error) {
	var sampler inspect.UsageSampler
	if h.opts.Runner != nil {
		sampler = h.opts.Runner
	}
	all, err := h.reader().AdminInstances(r.Context(), sampler)
	if err != nil {
		return nil, err
	}
	kindFilter := strings.TrimSpace(r.URL.Query().Get("kind"))
	toolFilter := strings.TrimSpace(r.URL.Query().Get("tool"))
	if kindFilter == "" && toolFilter == "" {
		return all, nil
	}
	out := make([]inspect.AdminInstance, 0, len(all))
	for _, v := range all {
		if kindFilter != "" && v.Inst.Kind != kindFilter {
			continue
		}
		if toolFilter != "" && v.Inst.Tool != toolFilter {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// adminStopInstance force-stops any user's instance.
//
// "Force" means what it says: no confirmation from the owner, because the
// operator is responding to something the owner may not know about (a runaway
// service eating a login node). The record is updated by the same StopService
// the reaper uses, so what the owner sees stays truthful.
func (h *Handler) adminStopInstance(w http.ResponseWriter, r *http.Request, id string) {
	if h.opts.Runner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "this gateway has no supervisor, so it cannot stop instances",
		})
		return
	}
	inst, err := h.reader().AdminInstance(id)
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	t, err := h.manifestForAdmin(inst.Tool)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := h.opts.Runner.StopService(r.Context(), t, inst); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Re-read: StopService records the terminal state and duration, and the
	// caller is looking at the result of its own action.
	saved, err := h.reader().AdminInstance(id)
	if err != nil {
		saved = inst
	}
	writeJSON(w, 200, map[string]any{"instance": inspect.ViewOfInstance(saved)})
}

// manifestForAdmin loads a tool package for an admin action. It deliberately
// skips the grant check: an operator acting on someone else's instance is not
// "using" the tool, and a tool that was un-granted must still be stoppable.
func (h *Handler) manifestForAdmin(id string) (*tool.Tool, error) {
	if h.opts.ToolsDir == "" {
		return nil, fmt.Errorf("no tool directory is configured on this host")
	}
	t, err := tool.Find(h.opts.ToolsDir, id)
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", id, err)
	}
	return t, nil
}

// ─────────────────────────────────────────────────────────────────────────
// 工具与授权
// ─────────────────────────────────────────────────────────────────────────

func (h *Handler) adminTools() ([]inspect.AdminTool, error) {
	return h.reader().AdminTools(h.opts.Policy)
}

// grantBody is the wire shape of a grant, mirroring `srcos grant set`.
type grantBody struct {
	Users        []string `json:"users"`
	Groups       []string `json:"groups"`
	Public       bool     `json:"public"`
	MaxCPU       int      `json:"maxCpu"`
	MaxMemory    string   `json:"maxMemory"`
	MaxInstances int      `json:"maxInstances"`
}

// adminSetGrant replaces one tool's grant, exactly as `srcos grant set` does.
func (h *Handler) adminSetGrant(w http.ResponseWriter, r *http.Request, username, toolID string) {
	if strings.TrimSpace(toolID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a tool id is required"})
		return
	}
	var body grantBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	policy := h.opts.Policy
	// A grant naming a group that does not exist would silently deny the very
	// user the operator meant to admit (the CLI refuses for the same reason).
	for _, grp := range body.Groups {
		if !containsString(policy.GroupNames(), grp) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf(
				"unknown group %q (existing: %v)", grp, policy.GroupNames())})
			return
		}
	}
	if !body.Public && len(body.Users) == 0 && len(body.Groups) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "this grant would admit nobody — " +
			"name users/groups/public, or DELETE the grant to deny everyone"})
		return
	}

	g := grant.Grant{
		Tool:   toolID,
		Users:  body.Users,
		Groups: body.Groups,
		Public: body.Public,
		Quota: grant.Quota{
			MaxCPU:       body.MaxCPU,
			MaxMemory:    body.MaxMemory,
			MaxInstances: body.MaxInstances,
		},
	}
	policy.SetGrant(g)
	if err := h.savePolicy(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.auditChange(r, agenttoken.HumanIdentity(username), "grant.set", "grant", toolID, map[string]any{
		"users":  strings.Join(g.Users, ","),
		"groups": strings.Join(g.Groups, ","),
		"public": g.Public,
	})
	writeJSON(w, 200, map[string]any{"grant": g})
}

func (h *Handler) adminRemoveGrant(w http.ResponseWriter, r *http.Request, username, toolID string) {
	if !h.opts.Policy.RemoveGrant(toolID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no grant for " + toolID})
		return
	}
	if err := h.savePolicy(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.auditChange(r, agenttoken.HumanIdentity(username), "grant.remove", "grant", toolID, nil)
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// adminSetGroup replaces a group's membership; an empty list removes the group
// (and with it every grant that named it, which the console warns about).
func (h *Handler) adminSetGroup(w http.ResponseWriter, r *http.Request, username string) {
	var body struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
	}
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a group name is required"})
		return
	}
	if len(body.Users) == 0 {
		// Removing a group that grants still name it would leave grants that
		// admit nobody — a silent revocation. Say so, and let the operator
		// decide by emptying or by removing the grants first.
		var stillUsed []string
		for _, g := range h.opts.Policy.Snapshot().Grants {
			if containsString(g.Groups, body.Name) {
				stillUsed = append(stillUsed, g.Tool)
			}
		}
		if len(stillUsed) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": fmt.Sprintf(
				"group %q is still named by the grants of %v — remove or edit those first", body.Name, stillUsed)})
			return
		}
	}
	h.opts.Policy.SetGroup(body.Name, body.Users)
	if err := h.savePolicy(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.auditChange(r, agenttoken.HumanIdentity(username), "group.set", "group", body.Name, map[string]any{
		"users": strings.Join(body.Users, ","),
	})
	writeJSON(w, 200, map[string]any{"groups": h.opts.Policy.Snapshot().Groups})
}

// adminSetAdmins replaces the admin list.
func (h *Handler) adminSetAdmins(w http.ResponseWriter, r *http.Request, username string) {
	var body struct {
		Admins []string `json:"admins"`
	}
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}
	// A policy with no admins has nobody who can put one back: that is a
	// lockout, not a configuration. (Editing grants.yaml by hand can still do
	// it, which is the documented escape hatch.)
	if len(body.Admins) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a policy with no admins has nobody " +
			"who can manage it — edit config/grants.yaml by hand if that is really what you want"})
		return
	}
	h.opts.Policy.SetAdmins(body.Admins)
	if err := h.savePolicy(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	h.auditChange(r, agenttoken.HumanIdentity(username), "admins.set", "policy", "admins", map[string]any{
		"admins": strings.Join(body.Admins, ","),
	})
	writeJSON(w, 200, map[string]any{"admins": h.opts.Policy.Snapshot().Admins})
}

// savePolicy persists the live policy.
func (h *Handler) savePolicy() error {
	if h.opts.PolicyPath == "" {
		return nil
	}
	if err := grant.Save(h.opts.PolicyPath, h.opts.Policy); err != nil {
		return fmt.Errorf("could not save the authorization policy: %w", err)
	}
	return nil
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
