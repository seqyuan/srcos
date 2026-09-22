package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// GrantChecker answers "may this user use this tool, and with how much".
//
// It is an interface rather than a concrete type so this package does not
// depend on how the policy is stored, and so a deployment can substitute a
// different model without touching the API surface.
type GrantChecker interface {
	Allowed(username, toolID string) bool
	QuotaFor(username, toolID string) grant.Quota
	IsAdmin(username string) bool
}

// RenderToolForm renders the generated fallback form for a tool. It is injected
// from the server so this package does not depend on the web layer.
type RenderToolForm func(username string, t *tool.Tool, storages []storage.Storage) string

// Options carries the seams the API needs beyond the user registry.
//
// Every field is optional: an empty ToolsDir disables the tool endpoints, and a
// nil Storages makes /api/paths answer 503 rather than pretend.
type Options struct {
	// ConfigDir holds the runtime state (data/instances, data/ws, ...).
	ConfigDir string
	// ToolsDir is the tool package root.
	ToolsDir string
	// Storages is the StorageProvider behind /api/paths.
	Storages storage.Provider
	// Grants filters the catalogue and gates execution. Nil means "authorization
	// is not wired" (a single-user deployment); the server substitutes a
	// deny-by-default policy when a grants file is expected but absent.
	Grants GrantChecker
	// RenderToolForm renders a tool's generated form page.
	RenderToolForm RenderToolForm
	// AgentTokens authenticates `Authorization: Bearer` requests, i.e. programs
	// (agents, MCP clients) rather than browsers (ADR-019). Nil means token
	// authentication is not wired and only session cookies are accepted.
	AgentTokens *agenttoken.Store
	// Policy is the authorization policy. Nil disables the management surface
	// (/api/admin/*): an admin is someone the policy says is one, so without a
	// policy there is no one to authorize.
	Policy *grant.Policy
	// PolicyPath is where the policy is persisted when the management surface
	// edits it. Empty means "edit in memory only", which is what a test wants.
	PolicyPath string
	// Runner is the supervisor, used by the management surface to stop any
	// user's instance and to sample what it is using.
	Runner *runtime.Runner
}

// Handler handles REST API requests for service management.
type Handler struct {
	Registry      *config.UserRegistry
	SessionSecret string
	opts          Options
}

// NewHandler creates a new API handler.
func NewHandler(registry *config.UserRegistry, sessionSecret string) *Handler {
	return NewHandlerWithOptions(registry, sessionSecret, Options{})
}

// NewHandlerWithOptions creates an API handler with the tool/storage seams wired.
func NewHandlerWithOptions(registry *config.UserRegistry, sessionSecret string, opts Options) *Handler {
	return &Handler{
		Registry:      registry,
		SessionSecret: sessionSecret,
		opts:          opts,
	}
}

// ServeHTTP implements http.Handler. Returns false if the request is not an API route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path

	// Only handle known API routes
	// The management surface is handled first: it is admin-only, and its routes
	// would otherwise be claimed by the generic /api/jobs-style dispatch below.
	if strings.HasPrefix(path, "/api/admin/") || path == "/api/admin" {
		// CSRF guard for cookie-authenticated writes happens below with every
		// other API route (the check is shared, not repeated per surface).
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !auth.SameOriginRequest(r) &&
			!agenttoken.HasBearerCredentials(r) {
			writeJSON(w, 403, map[string]string{"error": "cross-origin request rejected"})
			return true
		}
		return h.adminHandler(w, r)
	}

	isAPI := path == "/api/services" ||
		path == "/api/services/layout" ||
		strings.HasPrefix(path, "/api/services/") ||
		path == "/api/tools" ||
		strings.HasPrefix(path, "/api/tools/") ||
		path == "/api/paths" ||
		path == "/api/jobs"

	if !isAPI {
		return false
	}

	// CSRF guard: state-changing requests from browsers must carry an Origin
	// matching the gateway's own host. Browsers always send Origin on
	// POST/PUT/DELETE, so cross-site (and cross-host) pages are rejected.
	//
	// A bearer token is exempt: it is not an ambient credential (the browser
	// never attaches one by itself), so a cross-site page cannot ride on it,
	// and some MCP clients legitimately send their own Origin.
	if r.Method != http.MethodGet && r.Method != http.MethodHead &&
		!agenttoken.HasBearerCredentials(r) && !auth.SameOriginRequest(r) {
		writeJSON(w, 403, map[string]string{"error": "cross-origin request rejected"})
		return true
	}

	ident, ok := h.identity(w, r)
	if !ok {
		return true
	}

	// Scope gate. An agent token is a read credential in this phase: the write
	// surface (submit / cancel / run_flow) is the second half of ADR-019, and
	// no token can hold `submit` yet — so the check is here to make the rule
	// explicit at the one place that would have to change.
	if ident.Agent && !isReadMethod(r.Method) && !ident.Has(agenttoken.ScopeSubmit) {
		log.Printf("[srcos] agent %s refused (read-only token): %s %s", ident.Describe(), r.Method, r.URL.Path)
		writeJSON(w, 403, map[string]string{
			"error": "agent token is read-only (scope \"read\"); writing requires the \"submit\" scope, which is reserved for the second phase of ADR-019",
		})
		return true
	}
	username := ident.User

	switch {
	case path == "/api/services" && r.Method == "GET":
		h.handleListServices(w, r, username)
	case path == "/api/services" && r.Method == "POST":
		h.handleAddService(w, r, username)
	case path == "/api/services/layout" && r.Method == "PUT":
		h.handleUpdateLayout(w, r, username)
	case strings.HasPrefix(path, "/api/services/") && r.Method == "DELETE":
		id := strings.TrimPrefix(path, "/api/services/")
		h.handleDeleteService(w, username, id)
	case strings.HasPrefix(path, "/api/services/") && r.Method == "PUT":
		id := strings.TrimPrefix(path, "/api/services/")
		h.handleUpdateService(w, r, username, id)

	// Tool catalogue, machine-readable interface, and the path browser behind
	// the srcos-path-picker primitive control.
	case path == "/api/tools" && r.Method == "GET":
		h.handleListTools(w, username)
	case strings.HasPrefix(path, "/api/tools/") && r.Method == "GET":
		h.handleDescribeTool(w, username, strings.TrimPrefix(path, "/api/tools/"))
	case path == "/api/paths" && r.Method == "GET":
		h.handlePaths(w, r, username)

	// Submissions: the generated form, a tool's own UI, and the MCP submit tool
	// all funnel through the same validation and the same drop-box.
	case path == "/api/jobs" && r.Method == "POST":
		h.handleSubmitJob(w, r, username)
	case path == "/api/jobs" && r.Method == "GET":
		h.handleListJobs(w, r, username)
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}

	return true
}

// identity resolves who the request acts as: an agent token if one is
// presented, otherwise a browser session.
//
// A presented-but-invalid token is a hard failure rather than a fallback to
// the cookie: a program that sent a bad credential must be told so, not
// silently served as whatever browser session the caller happens to hold.
func (h *Handler) identity(w http.ResponseWriter, r *http.Request) (agenttoken.Identity, bool) {
	if h.opts.AgentTokens != nil {
		ident, err := h.opts.AgentTokens.Authenticate(r)
		switch {
		case err == nil:
			// The store has already checked that the token's user still exists
			// (AttachUserCheck); what is left here is the audit line.
			//
			// The audit line for every accepted agent call (ADR-019). It records
			// the authentication event — who acted, as which user, with which
			// scopes — because that is the fact an auditor asks for. Whether the
			// call was then allowed is decided (and, when refused, logged) by the
			// scope gate below.
			log.Printf("[srcos] agent %s %s %s", ident.Describe(), r.Method, r.URL.Path)
			return ident, true
		case errors.Is(err, agenttoken.ErrNoCredential):
			// No credential: fall through to the session cookie.
		default:
			writeJSON(w, 401, map[string]string{"error": err.Error()})
			return agenttoken.Identity{}, false
		}
	}

	session := h.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		writeJSON(w, 401, map[string]string{"error": "Unauthorized"})
		return agenttoken.Identity{}, false
	}
	return agenttoken.HumanIdentity(session.UserID), true
}

// isReadMethod reports whether an HTTP method only reads. Anything else is a
// write and needs a scope that can write.
func isReadMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// currentSessionRev returns the per-account session revision derived from the
// user's current password hash.
func (h *Handler) currentSessionRev(username string) string {
	if !config.IsValidUsername(username) {
		return ""
	}
	uc := h.Registry.GetUserConfigForLogin(username)
	if uc == nil {
		return ""
	}
	return auth.SessionRev(uc.Auth.PasswordHash)
}

// sessionFromCookies validates the session cookie, binding it to the user's
// current password-hash revision so password changes revoke old sessions.
func (h *Handler) sessionFromCookies(cookieHeader string) auth.SessionResult {
	cookies := auth.ParseCookies(cookieHeader)
	token := cookies[auth.SessionCookieName]
	if token == "" {
		return auth.SessionResult{}
	}
	userID, _, _, err := auth.SessionTokenParts(token)
	if err != nil || userID == "" {
		return auth.SessionResult{}
	}
	return auth.ValidateSessionToken(token, h.SessionSecret, h.currentSessionRev(userID))
}

func (h *Handler) getConfigPath(username string) (string, error) {
	user := h.Registry.GetUser(username)
	if user == nil {
		return "", fmt.Errorf("user config not found")
	}
	return user.ConfigPath, nil
}

func (h *Handler) handleListServices(w http.ResponseWriter, r *http.Request, username string) {
	user := h.Registry.GetUser(username)
	if user == nil {
		writeJSON(w, 200, map[string]interface{}{"services": []interface{}{}, "writable": false})
		return
	}

	writable := isWritable(user.ConfigPath)
	writeJSON(w, 200, map[string]interface{}{
		"services": user.Config.Services,
		"writable": writable,
	})
}

func (h *Handler) handleAddService(w http.ResponseWriter, r *http.Request, username string) {
	var body addServiceBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}

	// Reject empty names and oversized fields so a stray request cannot
	// create a nonsense "service" card or bloat the dashboard page.
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "name is required"})
		return
	}
	if len(name) > 200 || len(body.Description) > 500 || len(body.Category) > 100 {
		writeJSON(w, 400, map[string]string{"error": "field too long (name ≤ 200, description ≤ 500, category ≤ 100 chars)"})
		return
	}

	configPath, err := h.getConfigPath(username)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if !isWritable(configPath) {
		writeJSON(w, 403, map[string]string{"error": "config is not writable"})
		return
	}

	// WebSocket defaults to true unless explicitly disabled.
	ws := true
	if body.WebSocket != nil {
		ws = *body.WebSocket
	}

	svc := config.ServiceConfig{
		ID:          config.SlugifyName(name),
		Name:        name,
		Description: body.Description,
		Host:        body.Host,
		Port:        body.Port,
		BackendPath: body.BackendPath,
		WebSocket:   ws,
		Category:    body.Category,
		BWLimit:     body.BWLimit,
	}
	if body.Path != "" {
		svc.Path = config.NormalizePath(body.Path)
	}
	// An empty Path means config.AddService auto-generates a unique one from
	// the (also unique-ified) ID.

	added, err := config.AddService(configPath, svc)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	h.Registry.Reload()
	writeJSON(w, 201, map[string]interface{}{"service": added})
}

func (h *Handler) handleUpdateLayout(w http.ResponseWriter, r *http.Request, username string) {
	var body layoutBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}

	configPath, err := h.getConfigPath(username)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if !isWritable(configPath) {
		writeJSON(w, 403, map[string]string{"error": "config is not writable"})
		return
	}

	items := make([]config.LayoutItem, len(body.Items))
	for i, item := range body.Items {
		items[i] = config.LayoutItem{
			ID:       item.ID,
			Order:    item.Order,
			Category: item.Category,
		}
	}

	if err := config.UpdateServicesLayout(configPath, items); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	h.Registry.Reload()
	user := h.Registry.GetUser(username)
	services := []config.ServiceConfig{}
	if user != nil {
		services = user.Config.Services
	}
	writeJSON(w, 200, map[string]interface{}{"services": services})
}

func (h *Handler) handleDeleteService(w http.ResponseWriter, username, id string) {
	configPath, err := h.getConfigPath(username)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if !isWritable(configPath) {
		writeJSON(w, 403, map[string]string{"error": "config is not writable"})
		return
	}

	if err := config.RemoveService(configPath, id); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	h.Registry.Reload()
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

func (h *Handler) handleUpdateService(w http.ResponseWriter, r *http.Request, username, id string) {
	var body updateBody
	if err := parseBody(r, &body); err != nil {
		writeBodyError(w, err)
		return
	}

	// A provided name must be non-empty and bounded (mirrors handleAddService).
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			writeJSON(w, 400, map[string]string{"error": "name cannot be empty"})
			return
		}
		if len(name) > 200 {
			writeJSON(w, 400, map[string]string{"error": "name too long (≤ 200 chars)"})
			return
		}
	}
	// Description/category bounds apply even when name is not being changed.
	if (body.Description != nil && len(*body.Description) > 500) || (body.Category != nil && len(*body.Category) > 100) {
		writeJSON(w, 400, map[string]string{"error": "field too long (description ≤ 500, category ≤ 100 chars)"})
		return
	}

	configPath, err := h.getConfigPath(username)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if !isWritable(configPath) {
		writeJSON(w, 403, map[string]string{"error": "config is not writable"})
		return
	}

	update := config.ServiceUpdate{
		Name:        body.Name,
		Description: body.Description,
		Host:        body.Host,
		Port:        body.Port,
		Path:        body.Path,
		BackendPath: body.BackendPath,
		WebSocket:   body.WebSocket,
		Category:    body.Category,
		BWLimit:     body.BWLimit,
	}

	if err := config.UpdateService(configPath, id, update); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}

	h.Registry.Reload()
	user := h.Registry.GetUser(username)
	var updated *config.ServiceConfig
	if user != nil {
		for _, s := range user.Config.Services {
			if s.ID == id {
				updated = &s
				break
			}
		}
	}
	writeJSON(w, 200, map[string]interface{}{"service": updated})
}

// Request body types

type addServiceBody struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Host        string `json:"host,omitempty"`
	Port        int    `json:"port"`
	Path        string `json:"path,omitempty"`
	BackendPath string `json:"backend_path,omitempty"`
	WebSocket   *bool  `json:"websocket,omitempty"`
	Category    string `json:"category,omitempty"`
	BWLimit     int64  `json:"bwlimit,omitempty"`
}

type layoutBody struct {
	Items []layoutItemBody `json:"items"`
}

type layoutItemBody struct {
	ID       string `json:"id"`
	Order    int    `json:"order"`
	Category string `json:"category,omitempty"`
}

type updateBody struct {
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
	Host        *string `json:"host,omitempty"`
	Port        *int    `json:"port,omitempty"`
	Path        *string `json:"path,omitempty"`
	BackendPath *string `json:"backend_path,omitempty"`
	WebSocket   *bool   `json:"websocket,omitempty"`
	Category    *string `json:"category,omitempty"`
	BWLimit     *int64  `json:"bwlimit,omitempty"`
}

// Helpers

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// maxBodySize caps JSON request bodies. Bodies larger than this are rejected
// with 413 instead of being silently truncated mid-JSON.
const maxBodySize = 1 << 16 // 64KB

var errBodyTooLarge = fmt.Errorf("request body too large (max 64KB)")

func parseBody(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("missing body")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodySize+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(data) > maxBodySize {
		return errBodyTooLarge
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// writeBodyError maps a parseBody error to the right HTTP status: 413 for
// oversized bodies, 400 otherwise.
func writeBodyError(w http.ResponseWriter, err error) {
	if errors.Is(err, errBodyTooLarge) {
		writeJSON(w, 413, map[string]string{"error": "request body too large"})
		return
	}
	writeJSON(w, 400, map[string]string{"error": err.Error()})
}

func isWritable(path string) bool {
	return config.IsWritable(path)
}
