package server

import (
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

import (
	"github.com/seqyuan/srcos/internal/accessrequest"
	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/api"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/mcp"
	"github.com/seqyuan/srcos/internal/resource"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
	"github.com/seqyuan/srcos/internal/web"
)

// This file renders SRCOS's own pages (dashboard, tool forms, viewer, tasks,
// admin) and serves its embedded assets. They are server-side Go templates on
// purpose: a page must work with JavaScript disabled (ADR-012 keeps React for
// the canvas alone).

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))

	// Refresh session if needed
	if session.Valid && session.UserID != "" && auth.ShouldRefreshSession(session, s.state.Auth.SessionTTL) {
		secure := auth.IsSecureRequest(r)
		cookie := s.issueSessionCookie(session.UserID, secure)
		w.Header().Add("Set-Cookie", cookie)
	}

	if session.Valid && session.UserID != "" {
		user := s.registry.GetUser(session.UserID)
		services := []config.ServiceConfig{}
		writable := false
		twoFAEnabled := false
		if user != nil {
			services = user.Config.Services
			writable = config.IsWritable(user.ConfigPath)
			twoFAEnabled = user.Config.Auth.TOTPSecret != ""
		}
		admin := false
		if policy, ok := s.grants.(*grant.Policy); ok {
			admin = policy.IsAdmin(session.UserID)
		}
		sendHTML(w, 200, web.DashboardPage(s.siteTitle, session.UserID, services, writable, twoFAEnabled, admin))
	} else {
		sendHTML(w, 200, web.LoginPage(s.siteTitle, "", ""))
	}
}

// TaskQueue exposes the queue for the admin surface (in-flight count) and for
// tests.
func (s *Server) TaskQueue() *execute.Queue { return s.taskQueue }

// UserCount returns the number of loaded users.
func (s *Server) UserCount() int {
	return len(s.registry.ListUsers())
}

func sendHTML(w http.ResponseWriter, status int, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Gateway-owned pages must not be framed by unrelated sites (clickjacking
	// of the login/dashboard) or sniffed as a different content type.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	io.WriteString(w, html)
}

// handleAsset serves the embedded front-end assets.
//
// Only a fixed allowlist is served: an asset route that took a path would be a
// file-read primitive sitting in front of the whole filesystem.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/assets/srcos-path-picker.js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = io.WriteString(w, web.PathPickerJS)
	default:
		http.NotFound(w, r)
	}
}

// handleAdminPage renders the management console.
//
// The page's data comes from the same read model the API serves (inspect), so
// the console and the API cannot disagree — and the console works with
// JavaScript disabled, because the tables are rendered here.
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" && r.URL.Path != "/admin/" {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	policy, _ := s.grants.(*grant.Policy)
	if policy == nil || !policy.IsAdmin(username) {
		// Not a secret, just not theirs: the dashboard is the honest answer.
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	var sampler inspect.UsageSampler
	if s.runner != nil {
		sampler = s.runner
	}
	instances, err := s.reader().AdminInstances(r.Context(), sampler)
	if err != nil {
		log.Printf("[srcos] admin: %v", err)
	}
	tools, terr := s.reader().AdminTools(policy)
	if terr != nil {
		log.Printf("[srcos] admin: %v", terr)
	}
	snapshot := policy.Snapshot()

	var users []string
	for _, u := range s.registry.ListUsers() {
		users = append(users, u.Username)
	}
	// Pending access requests (B3). A read failure must not blank the console,
	// so it is logged and the section reads "none".
	var pending []accessrequest.Request
	if all, rerr := accessrequest.List(s.dataDir()); rerr != nil {
		log.Printf("[srcos] admin requests: %v", rerr)
	} else {
		for _, req := range all {
			if req.State == accessrequest.Pending {
				pending = append(pending, req)
			}
		}
	}

	sendHTML(w, 200, web.AdminPage(s.siteTitle, username, web.AdminData{
		Instances:    instances,
		Tools:        tools,
		Admins:       snapshot.Admins,
		Groups:       snapshot.Groups,
		Users:        users,
		Requests:     pending,
		DefaultAllow: snapshot.DefaultAllow,
	}))
}

// handleAdminSubPage serves the console's sub-pages (/admin/...).
func (s *Server) handleAdminSubPage(w http.ResponseWriter, r *http.Request) {
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	policy, _ := s.grants.(*grant.Policy)
	if policy == nil || !policy.IsAdmin(username) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// /admin/flows/<id>/edit is the canvas: the one page served from the built
	// frontend. Everything else under /admin/ is the console itself.
	rest := strings.TrimPrefix(r.URL.Path, "/admin/")
	if id, ok := strings.CutPrefix(rest, "flows/"); ok {
		if id, ok := strings.CutSuffix(id, "/edit"); ok && flow.ValidFlowID(id) {
			sendHTML(w, 200, web.FlowEditorPage(s.siteTitle, username, id))
			return
		}
	}
	if rest == "audit" || rest == "audit/" {
		s.handleAdminAuditPage(w, r, username)
		return
	}
	http.NotFound(w, r)
}

// handleAdminAuditPage renders the audit console (ADR-024). Reading it is
// admin-only, like every /admin page; writing is the platform's own business.
func (s *Server) handleAdminAuditPage(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	filter := audit.Filter{
		User:     strings.TrimSpace(q.Get("user")),
		Action:   strings.TrimSpace(q.Get("action")),
		Decision: strings.TrimSpace(q.Get("decision")),
	}
	if since := strings.TrimSpace(q.Get("since")); since != "" {
		at, err := audit.ParseSince(since)
		if err != nil {
			sendHTML(w, http.StatusBadRequest, web.NoticePage(s.siteTitle, "筛选条件无效",
				err.Error(), "/admin/audit", "返回审计流"))
			return
		}
		filter.Since = at
	}
	events, err := audit.Query(s.dataDir(), filter)
	if err != nil {
		log.Printf("[srcos] audit read: %v", err)
		sendHTML(w, http.StatusInternalServerError, web.NoticePage(s.siteTitle, "读取失败",
			err.Error(), "/admin/audit", "返回审计流"))
		return
	}
	// Tamper-evidence status for the banner: does the stream still verify?
	problems, verr := audit.Verify(s.dataDir())
	if verr != nil {
		log.Printf("[srcos] audit verify: %v", verr)
	}
	sendHTML(w, 200, web.AuditPage(s.siteTitle, username, events, filter, problems, s.audit.ForwardStatus()))
}

// dataDir is where runtime state lives (data/), a sibling of config/. The
// audit stream, instance records and workspaces are all under it.
func (s *Server) dataDir() string {
	return config.DataDir(config.DirOf(s.registry))
}

// handleUI serves the built frontend (ADR-012: embedded, same binary).
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	assets := web.UIAssets()
	if assets == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" || strings.Contains(name, "..") {
		name = "index.html"
	}
	if info, err := fs.Stat(assets, name); err != nil || info.IsDir() {
		// A single-page app: unknown paths fall back to its shell.
		name = "index.html"
	}
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Vite emits content-hashed asset names, so they can be cached hard; the
	// shell must not be, or a rebuilt bundle would not be picked up.
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

// contentTypeFor maps an extension to a content type. The set is deliberately
// small: the gateway serves only what a built bundle contains.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"), strings.HasSuffix(name, ".mjs"):
		return "application/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	default:
		return "text/html; charset=utf-8"
	}
}

// mcpHandler is the gateway's MCP endpoint: the read-only platform surface an
// agent reaches with an agent token (ADR-019).
func (s *Server) mcpHandler() http.Handler {
	// The write path is the API handler's own controller, so an agent's
	// submission and a browser's form submission are the same operation with the
	// same checks (one implementation, two front-ends).
	return mcp.NewServer(s.mcpVersion, s.reader(), s.authenticator, s.apiHandler.Executor())
}

// requireUserPage enforces a session for a page and redirects to login
// otherwise, preserving where the user was going.
func (s *Server) requireUserPage(w http.ResponseWriter, r *http.Request) (string, bool) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if session.Valid && session.UserID != "" {
		return session.UserID, true
	}
	next := r.URL.RequestURI()
	if !validLoginNext(next) {
		next = "/"
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
	return "", false
}

// handleToolsPage lists the tools a user may use.
func (s *Server) handleToolsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/tools" && r.URL.Path != "/tools/" {
		s.handleToolFormPage(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	tools, err := s.visibleTools(username)
	if err != nil {
		log.Printf("[srcos] tools: %v", err)
		sendHTML(w, 500, web.NotFoundPage(s.siteTitle))
		return
	}
	// Tools the user cannot use but may ask for (B3). A read failure here must
	// not hide the tools they *can* use, so it is logged and the list is empty.
	requestable, rerr := s.reader().RequestableTools(username)
	if rerr != nil {
		log.Printf("[srcos] requestable tools: %v", rerr)
	}
	sendHTML(w, 200, web.ToolsPage(s.siteTitle, username, tools, requestable))
}

// handleRequestsPage shows a user their own access requests and their outcome.
func (s *Server) handleRequestsPage(w http.ResponseWriter, r *http.Request) {
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	reqs, err := accessrequest.ListFor(s.dataDir(), username)
	if err != nil {
		log.Printf("[srcos] requests: %v", err)
		sendHTML(w, 500, web.NotFoundPage(s.siteTitle))
		return
	}
	sendHTML(w, 200, web.RequestsPage(s.siteTitle, username, reqs))
}

// handleToolFormPage renders a tool's generated form.
func (s *Server) handleToolFormPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/tools/")
	if id == "" || strings.ContainsAny(id, "/\\") {
		http.NotFound(w, r)
		return
	}
	tools, err := s.visibleTools(username)
	if err != nil {
		log.Printf("[srcos] tools: %v", err)
		sendHTML(w, 500, web.NotFoundPage(s.siteTitle))
		return
	}
	for _, t := range tools {
		if t.ID != id {
			continue
		}
		// A service page is also its lifecycle surface: show the caller's own
		// instance (there is at most one per user+tool), so the page can offer
		// "open" and "stop" without a second request.
		var inst *inspect.InstanceView
		if t.Kind == tool.KindService {
			if views, verr := s.reader().Instances(username, t.ID, string(tool.KindService)); verr == nil && len(views) > 0 {
				inst = &views[0]
			}
		}
		sendHTML(w, 200, web.ToolFormPage(s.siteTitle, username, t, s.storagesForTool(t), inst))
		return
	}
	sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
}

// handleView renders one srcos:// resource, or — with no src — the roots this
// user may browse.
//
// It is the platform's own viewer (ADR-016's default layer): server-rendered,
// no JavaScript, no build step, so it works on a deployment that has never seen
// Node, and so a file is viewable even if a tool ships no UI of its own.
func (s *Server) handleView(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	reader := s.reader()
	src := strings.TrimSpace(r.URL.Query().Get("src"))

	if src == "" {
		scopes, err := reader.ResourceScopes(username)
		if err != nil {
			sendHTML(w, api.ErrorStatus(err), web.ResourceErrorPage(s.siteTitle, err.Error()))
			return
		}
		sendHTML(w, 200, web.ResourceScopesPage(s.siteTitle, scopes))
		return
	}

	addr, err := resource.Parse(src)
	if err != nil {
		sendHTML(w, http.StatusBadRequest, web.ResourceErrorPage(s.siteTitle, err.Error()))
		return
	}
	view, err := reader.ViewResource(username, inspect.ResourceRequest{
		Scope: addr.Scope,
		Path:  addr.Path,
		Tool:  addr.Tool,
	}, r.URL.Query().Get("viewer"))
	if err != nil {
		sendHTML(w, api.ErrorStatus(err), web.ResourceErrorPage(s.siteTitle, err.Error()))
		return
	}

	sendHTML(w, 200, web.ResourcePage(web.ResourcePageData{
		SiteTitle: s.siteTitle,
		Src:       addr.String(),
		Addr:      addr,
		View:      view,
		ScopeName: s.resourceScopeName(username, addr),
		Viewers:   resource.Default().All(),
	}))
}

// handleTokensPage renders the self-service agent-token page (ADR-019).
//
// The list is rendered here so credential management works without JavaScript;
// only "create" needs a script, because the plaintext must never travel back
// through a redirect or a query string.
func (s *Server) handleTokensPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	ids := make([]string, 0, 8)
	if tools, err := s.visibleTools(username); err == nil {
		for _, t := range tools {
			ids = append(ids, t.ID)
		}
	}
	sendHTML(w, 200, web.TokenPage(s.siteTitle, username, s.tokenRows(username), ids, agenttoken.KnownScopesNames()))
}

// tokenRows renders one user's tokens for the page, from the one view builder
// the API also uses (agenttoken.Store.ViewsFor).
func (s *Server) tokenRows(user string) []web.TokenRow {
	if s.agentTokens == nil {
		return nil
	}
	views := s.agentTokens.ViewsFor(user, time.Now())
	out := make([]web.TokenRow, 0, len(views))
	for _, v := range views {
		out = append(out, web.TokenRow{
			ID:       v.ID,
			Label:    v.Label,
			Scopes:   strings.Join(v.Scopes, ","),
			Tools:    strings.Join(v.Tools, ","),
			Created:  localTime(v.CreatedAt),
			Expires:  orNever(v.ExpiresAt),
			LastUsed: orDashTime(v.LastUsed),
			Status:   v.Status,
			Expired:  v.Status == "expired",
		})
	}
	return out
}

// localTime formats an instant for the page; the zero value prints as "—".
func localTime(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "—"
	}
	return at.Local().Format("2006-01-02 15:04")
}

func orNever(at *time.Time) string {
	if at == nil || at.IsZero() {
		return "never"
	}
	return at.Local().Format("2006-01-02 15:04")
}

func orDashTime(at *time.Time) string { return localTime(at) }

// resourceScopeName finds the human title of the scope an address names, so the
// viewer's heading says "集群共享盘" rather than "data".
func (s *Server) resourceScopeName(username string, addr resource.Address) string {
	scopes, err := s.reader().ResourceScopes(username)
	if err == nil {
		for _, sc := range scopes {
			if sc.Scope == addr.Scope && sc.Tool == addr.Tool {
				return sc.Name
			}
		}
	}
	return addr.Scope
}

// handleTasksPage lists the user's own instances (tasks and services).
//
// It reads through the same inspect reader as /api/jobs, so the page and the
// API cannot disagree about what this user may see — and the page works with
// JavaScript disabled, because the table is rendered here.
func (s *Server) handleTasksPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	toolFilter := strings.TrimSpace(r.URL.Query().Get("tool"))
	kindFilter := strings.TrimSpace(r.URL.Query().Get("kind"))

	views, err := s.reader().Instances(username, toolFilter, kindFilter)
	if err != nil {
		sendHTML(w, api.ErrorStatus(err), web.NoticePage(s.siteTitle, "读不到任务列表", err.Error(), "/", "返回仪表盘"))
		return
	}
	tools, err := s.visibleTools(username)
	if err != nil {
		log.Printf("[srcos] tasks: %v", err)
		tools = nil
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.ID)
	}
	sendHTML(w, 200, web.TasksPage(s.siteTitle, views, names, toolFilter, kindFilter))
}

// handleTaskPage renders one instance: metadata, artifacts, and the log tail.
//
// The tail is rendered server-side so the page is readable without JavaScript;
// the script in the page then streams new lines from
// /api/jobs/<id>/logs?follow=1 and appends them.
func (s *Server) handleTaskPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	username, ok := s.requireUserPage(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/tasks/")
	if id == "" || strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		http.NotFound(w, r)
		return
	}
	reader := s.reader()
	view, others, err := reader.Instance(username, id)
	if err != nil {
		msg := err.Error()
		if len(others) > 0 {
			msg += " 候选项：" + strings.Join(others, "、")
		}
		sendHTML(w, api.ErrorStatus(err), web.NoticePage(s.siteTitle, "找不到这个实例", msg, "/tasks", "返回任务列表"))
		return
	}
	artifacts, err := reader.Artifacts(username, view.ID)
	if err != nil {
		sendHTML(w, api.ErrorStatus(err), web.NoticePage(s.siteTitle, "读不到产物", err.Error(), "/tasks", "返回任务列表"))
		return
	}
	// A missing log is a normal state (a task that has not started); the page
	// says so instead of failing.
	tail, err := reader.Logs(username, view.ID, 200)
	if err != nil {
		tail = ""
	}
	sendHTML(w, 200, web.TaskPage(s.siteTitle, *view, artifacts, tail))
}

// reader is the read-only view of the platform, shared with the API and MCP
// front-ends (ADR-018). The pages ask it the same questions they used to answer
// from their own copy of the grant filter.
func (s *Server) reader() *inspect.Reader {
	return &inspect.Reader{
		ConfigDir: config.DirOf(s.registry),
		ToolsDir:  s.toolsDir,
		Storages:  s.storages,
		Grants:    s.grants,
		// "Can this tool be requested?" is only answerable by the policy
		// itself; a policy-less deployment advertises nothing for request (B3).
		Requestable: requestabilityOf(s.grants),
	}
}

// requestabilityOf narrows a grant checker to the requestability question, when
// the concrete policy can answer it.
func requestabilityOf(g api.GrantChecker) inspect.Requestability {
	if p, ok := g.(*grant.Policy); ok {
		return p
	}
	return nil
}

// visibleTools applies the grant filter through the shared read side.
func (s *Server) visibleTools(username string) ([]*tool.Tool, error) {
	return s.reader().VisibleManifests(username)
}

func (s *Server) storagesForTool(t *tool.Tool) []storage.Storage {
	return s.reader().StorageDefs(t)
}
