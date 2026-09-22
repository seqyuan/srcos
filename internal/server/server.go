package server

import (
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/seqyuan/srcos/internal/activity"
	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/api"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/mcp"
	"github.com/seqyuan/srcos/internal/proxy"
	"github.com/seqyuan/srcos/internal/rate"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
	"github.com/seqyuan/srcos/internal/web"
)

const (
	loginErrorMsg     = "用户名或密码错误"
	pendingTTLSeconds = 300 // pending 2FA token lifetime (5 minutes)
)

// Server is the main SRCOS HTTP server.
type Server struct {
	state         *config.StateConfig
	registry      *config.UserRegistry
	sessionSecret string
	siteTitle     string
	toolsDir      string
	storages      storage.Provider
	grants        api.GrantChecker
	loginLimiter  *rate.Limiter
	totpLimiter   *rate.Limiter
	apiHandler    *api.Handler
	transport     *http.Transport
	// agentTokens is the program-credential store: the API and the MCP
	// endpoint both authenticate through it (ADR-019).
	agentTokens *agenttoken.Store
	// mcpVersion is the build version reported in MCP's initialize.
	mcpVersion string
	// routes is the dynamic routing table: the only coupling point between the
	// orchestration layer (which publishes service endpoints) and this proxy
	// layer (ADR-003). It is a cache of "what is reachable right now", rebuilt
	// from the instance records at startup and on every scan tick.
	routes *route.Table
	// serviceActivity records when each instance was last *used*, which only
	// the proxy can see (the record knows when a service started, not whether
	// anyone is still working in it).
	serviceActivity *activity.Journal
	// activeConns counts open WebSocket/tunneled connections per instance: an
	// open notebook is not idle, whatever the clock says (ADR-015).
	activeConns *proxy.ActiveConns
	// runner is the supervisor: it re-adopts instances after a restart and
	// reclaims the ones whose lifetime is over. Nil means this process only
	// serves (a read-only or single-purpose deployment).
	runner *runtime.Runner
	// flowsDir is the flow package root (the canvas reads and writes it).
	flowsDir string
	// grantsPath is where the authorization policy lives, watched for hand
	// edits; policyMtime is the last version read (guarded by policyMu).
	grantsPath  string
	policyMtime time.Time
	policyMu    sync.Mutex
	// reaper enforces the tool's lifecycle ceilings via the same runner.
	reaper *runtime.Reaper
}

// Options carries the seams the gateway needs beyond the user registry:
// where tool packages live, and which shared data is declared.
//
// A deployment with neither still works — the tool pages and /api/paths simply
// report that they are not configured, rather than pretending.
type Options struct {
	// ToolsDir is the tool package root. Empty means "look beside the binary".
	ToolsDir string
	// Storages is the StorageProvider. Nil means "load config/storages.yaml".
	Storages storage.Provider
	// Grants gates tool visibility and execution. Nil means "allow all", which
	// is the pre-authorization default and is replaced in Phase 3.
	Grants api.GrantChecker
	// Runner executes tool instances (used by the admin surface in Phase 3).
	Runner *runtime.Runner
	// FlowsDir is the flow package root (for the admin console's canvas).
	// Empty means "look beside the binary".
	FlowsDir string
	// Version is the build version reported to MCP clients in `initialize`.
	// Empty means "dev".
	Version string
	// Routes is the dynamic routing table. When set (the gateway builds one
	// runner for the whole process) it must be the *same* table the runner
	// publishes into, or the proxy would read a table nobody writes.
	Routes *route.Table
	// Supervisor, when set, is the runner the gateway uses to re-adopt live
	// instances at startup and to reclaim expired ones on the scan tick. It
	// must not carry a user: reconciling and reaping act on records, not on
	// behalf of an actor (package runtime refuses to start units without one).
	Supervisor *runtime.Runner
}

// New creates a new Server from state config. configDir is where the shared
// user configs live (configDir/users/*.yaml).
func New(state *config.StateConfig, configDir string) *Server {
	return NewWithOptions(state, configDir, Options{})
}

// NewWithOptions creates a Server with the tool and storage seams wired.
func NewWithOptions(state *config.StateConfig, configDir string, opts Options) *Server {
	// Forwarded headers are only trusted from the configured proxy; with no
	// trusted proxy configured, client IPs and HTTPS detection use the TCP
	// peer directly (X-Forwarded-* is spoofable by any direct client).
	auth.ConfigureTrustedProxy(state.Server.TrustedProxy)

	// Record the gateway's own listen port so backend targets pointing back
	// at it can be rejected as self-loops (write-time and dial-time).
	config.SetGatewayListenPort(state.Server.Port)

	registry := config.NewUserRegistry(configDir)
	registry.Reload()

	// Site title shown in the top-left corner of the dashboard and login
	// page; falls back to the product name when unset.
	siteTitle := state.Server.Title
	if siteTitle == "" {
		siteTitle = "SRCOS"
	}

	toolsDir := opts.ToolsDir
	if toolsDir == "" {
		toolsDir = config.ResolveToolsDir(configDir)
	}
	flowsDir := opts.FlowsDir
	if flowsDir == "" {
		flowsDir = config.ResolveFlowsDir(configDir)
	}
	mcpVersion := opts.Version
	if mcpVersion == "" {
		mcpVersion = "dev"
	}
	routes := opts.Routes
	if routes == nil {
		routes = route.NewTable()
	}
	storages := opts.Storages
	if storages == nil {
		provider, err := storage.Load(config.StoragesPath(configDir))
		if err != nil {
			log.Printf("[srcos] storages.yaml: %v (continuing with no shared data)", err)
		} else {
			storages = provider
		}
	}
	if checker, ok := storages.(storage.ReachabilityChecker); ok {
		for _, problem := range checker.CheckReachable() {
			log.Printf("[srcos] warning: %s", problem)
		}
	}

	// The authorization policy. A configured-or-defaulted policy is always
	// present, because "no policy" and "policy allowing everything" must not be
	// the same state: a deployment that forgot the file should deny, not open.
	grants := opts.Grants
	if grants == nil {
		policy, err := grant.Load(config.GrantsPath(configDir))
		if err != nil {
			log.Printf("[srcos] grants.yaml: %v — denying every tool until it is fixed", err)
			// A malformed policy must not become an open policy.
			policy, _ = grant.New(nil, nil, nil)
		}
		grants = policy
	}
	if policy, ok := grants.(*grant.Policy); ok && len(policy.Grants) == 0 && !policy.DefaultAllow {
		log.Printf("[srcos] no grants declared — every tool is invisible except to admins %v", policy.Admins)
	}
	// Report tools nobody can reach, because the symptom of a missing grant is
	// a user seeing an empty catalogue rather than an error.
	if policy, ok := grants.(*grant.Policy); ok {
		if tools, err := tool.Discover(toolsDir); err == nil {
			var ungranted []string
			for _, t := range tools {
				if !policy.ReachesAnyone(t.ID) {
					ungranted = append(ungranted, t.ID)
				}
			}
			if len(ungranted) > 0 {
				log.Printf("[srcos] tools with no grant (invisible to non-admins): %v", ungranted)
			}
		}
	}

	// Agent tokens: program credentials for the agent / MCP surface (ADR-019).
	// The file holds hashes only, and a broken one leaves the store empty —
	// fail closed, never "keep the previous snapshot alive".
	agentTokens := agenttoken.New(config.AgentTokensPath(configDir))
	if err := agentTokens.Reload(); err != nil {
		log.Printf("[srcos] agent-tokens.yaml: %v — rejecting every agent token until it is fixed", err)
	}
	// Usage is gateway-written runtime state in data/, deliberately separate
	// from the CLI-written registry.
	agentTokens.AttachUsage(agenttoken.LoadUsage(config.AgentTokenUsagePath(configDir)))

	// Which service instances are actually being used. The reaper needs it:
	// idleTTL must measure traffic, not time since start.
	serviceActivity := activity.Load(config.ServiceActivityPath(configDir), serviceActivityHeader)
	// A credential must not outlive its account: the store refuses a token
	// whose user is gone, and every front-end that authenticates through it
	// (the REST API, MCP) gets that for free.
	agentTokens.AttachUserCheck(registry)
	if n := len(agentTokens.Tokens()); n > 0 {
		log.Printf("[srcos] %d agent token(s) loaded (Authorization: Bearer)", n)
	}

	// The management surface needs the *concrete* policy (it mutates it) and the
	// supervisor (it stops instances); a substituted GrantChecker leaves it
	// disabled rather than half-working.
	policy, _ := grants.(*grant.Policy)

	apiOpts := api.Options{
		ConfigDir:   configDir,
		ToolsDir:    toolsDir,
		Storages:    storages,
		Grants:      grants,
		AgentTokens: agentTokens,
		Policy:      policy,
		PolicyPath:  config.GrantsPath(configDir),
		Runner:      opts.Supervisor,
		FlowsDir:    flowsDir,
		RenderToolForm: func(username string, t *tool.Tool, sts []storage.Storage) string {
			return web.ToolFormPage(siteTitle, username, t, sts)
		},
	}

	srv := &Server{
		state:         state,
		registry:      registry,
		sessionSecret: state.Auth.SessionSecret,
		siteTitle:     siteTitle,
		toolsDir:      toolsDir,
		flowsDir:      flowsDir,
		storages:      storages,
		grants:        grants,
		agentTokens:   agentTokens,
		mcpVersion:    mcpVersion,
		loginLimiter:  rate.NewLimiter(10, 15*time.Minute),
		totpLimiter:   rate.NewLimiter(10, 15*time.Minute),
		apiHandler:    api.NewHandlerWithOptions(registry, state.Auth.SessionSecret, apiOpts),
		// A shared transport lets connection pools be reused across requests
		// instead of creating a fresh one per proxy request. SafeDialContext
		// re-validates backend addresses (and pins the validated IP) at dial
		// time, so the write-time host allowlist cannot be bypassed by a
		// DNS-rebinding / TOCTOU change to the hostname.
		transport: &http.Transport{
			DialContext:           config.SafeDialContext,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
		},
		routes:          routes,
		grantsPath:      config.GrantsPath(configDir),
		serviceActivity: serviceActivity,
		activeConns:     proxy.NewActiveConns(),
		runner:          opts.Supervisor,
	}
	if srv.runner != nil {
		srv.reaper = &runtime.Reaper{
			Runner: srv.runner,
			// Traffic is only observable here, and an open WebSocket is the
			// strongest form of "in use".
			ActiveWS:   srv.activeConns.Active,
			LastActive: srv.serviceActivity.Last,
		}
	}
	// Bring the records and the routing table in line with reality before
	// serving: adopt the instances that are still alive (they were started by
	// the CLI, in another process), mark the dead ones stopped, and publish the
	// survivors so a gateway restart does not take every service offline.
	srv.reconcile()
	return srv
}

// currentSessionRev returns the per-account session revision derived from the
// user's current password hash. Because the revision is embedded in session
// tokens, changing the password revokes all previously-issued sessions.
func (s *Server) currentSessionRev(username string) string {
	if !config.IsValidUsername(username) {
		return ""
	}
	uc := s.registry.GetUserConfigForLogin(username)
	if uc == nil {
		return ""
	}
	return auth.SessionRev(uc.Auth.PasswordHash)
}

// issueSessionCookie signs a session cookie bound to the user's current
// password-hash revision.
func (s *Server) issueSessionCookie(username string, secure bool) string {
	return auth.SetSessionCookie(s.sessionSecret, s.state.Auth.SessionTTL, username, s.currentSessionRev(username), secure)
}

// sessionFromCookies validates the session cookie, binding it to the user's
// current password-hash revision.
func (s *Server) sessionFromCookies(cookieHeader string) auth.SessionResult {
	cookies := auth.ParseCookies(cookieHeader)
	token := cookies[auth.SessionCookieName]
	if token == "" {
		return auth.SessionResult{}
	}
	userID, _, _, err := auth.SessionTokenParts(token)
	if err != nil || userID == "" {
		return auth.SessionResult{}
	}
	return auth.ValidateSessionToken(token, s.sessionSecret, s.currentSessionRev(userID))
}

// Handler returns the HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The MCP endpoint (ADR-019). It is a gateway route, like /api/*: a backend
	// service that happened to use /mcp keeps working under its own prefix
	// (/proxy/<user>/<service>/mcp), so the reserved path costs nothing.
	//
	// Both spellings are registered because a client configured with a trailing
	// slash should reach the endpoint, not a proxied backend (or a 404).
	mcpHandler := s.mcpHandler()
	mux.Handle(mcp.Endpoint, mcpHandler)
	mux.Handle(mcp.Endpoint+"/", mcpHandler)

	// API routes (handled by api.Handler)
	// Use specific exact matches to avoid conflicts with backend service APIs
	mux.HandleFunc("/api/services", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.Method == "POST" {
			s.apiHandler.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	})

	mux.HandleFunc("/api/services/", func(w http.ResponseWriter, r *http.Request) {
		// Handle /api/services/{id} and /api/services/layout
		path := r.URL.Path
		if path == "/api/services/layout" || strings.HasPrefix(path, "/api/services/") {
			if s.apiHandler.ServeHTTP(w, r) {
				return
			}
		}
		// Not a srcos API: try forwarding to backend service. Referer first:
		// an SPA page's API calls carry the page that issued them, which is a
		// more precise target than the route cookie (most recently visited).
		if s.handleRefererProxy(w, r) {
			return
		}
		if s.handleRouteCookieProxy(w, r) {
			return
		}
		if s.handleDefaultServiceProxy(w, r) {
			return
		}
		http.NotFound(w, r)
	})

	// Other /api/* paths: forward to backend services (e.g., Next.js API routes)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		// Skip if it's a srcos API path (already handled above)
		if r.URL.Path == "/api/services" || strings.HasPrefix(r.URL.Path, "/api/services/") {
			http.NotFound(w, r)
			return
		}
		// Referer first (the issuing page is the precise target), then route
		// cookie, then the user's default service as a last resort.
		if s.handleRefererProxy(w, r) {
			return
		}
		if s.handleRouteCookieProxy(w, r) {
			return
		}
		if s.handleDefaultServiceProxy(w, r) {
			return
		}
		http.NotFound(w, r)
	})

	// Favicon
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write([]byte(web.FaviconSVG()))
	})

	// Logout
	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", auth.ClearSessionCookie())
		// Route cookies outlive the session and would keep forwarding bare
		// paths to backends after logout; clear every one the browser holds.
		for _, c := range auth.ClearRouteCookies(r.Header.Get("Cookie")) {
			w.Header().Add("Set-Cookie", c)
		}
		http.Redirect(w, r, "/", http.StatusFound)
	})

	// Two-factor authentication (second login step)
	mux.HandleFunc("/login/2fa", func(w http.ResponseWriter, r *http.Request) {
		s.handleTwoFA(w, r)
	})

	// TOTP enrollment + management (requires an active session)
	mux.HandleFunc("/2fa/setup", func(w http.ResponseWriter, r *http.Request) {
		s.handleTwoFASetup(w, r)
	})
	mux.HandleFunc("/2fa/qr", func(w http.ResponseWriter, r *http.Request) {
		s.handleTwoFAQr(w, r)
	})
	mux.HandleFunc("/2fa/disable", func(w http.ResponseWriter, r *http.Request) {
		s.handleTwoFADisable(w, r)
	})

	// Login
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			// Login CSRF guard: a cross-site page must not be able to log the
			// victim's browser into the gateway (login CSRF). Browsers send
			// Origin on POST; non-browser clients without Origin pass.
			if !auth.SameOriginRequest(r) {
				sendHTML(w, 403, web.LoginPage(s.siteTitle, "登录请求被拒绝", ""))
				return
			}
			if s.handleBackendLogin(w, r) {
				return
			}
			s.handleLogin(w, r)
			return
		}
		// GET /login: may be a backend service login page (e.g. AnnoVibe redirect).
		// Only forward when a valid session exists; otherwise a stale route
		// cookie would hijack the SRCOS login page after logout.
		session := s.sessionFromCookies(r.Header.Get("Cookie"))
		if session.Valid && session.UserID != "" {
			if s.handleRouteCookieProxy(w, r) {
				return
			}
			if s.handleRefererProxy(w, r) {
				return
			}
		}
		s.handleLoginPage(w, r)
	})

	// Proxy routes
	mux.HandleFunc("/proxy/", func(w http.ResponseWriter, r *http.Request) {
		s.handleProxy(w, r)
	})

	// Tool/storage API. Registered as exact paths, like /api/services, so a
	// proxied backend's own /api/... tree is not shadowed by a catch-all.
	// These four are now reserved gateway paths (README「保留路径」).
	for _, p := range []string{"/api/tools", "/api/tools/", "/api/paths", "/api/jobs", "/api/admin", "/api/admin/"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			if s.apiHandler.ServeHTTP(w, r) {
				return
			}
			http.NotFound(w, r)
		})
	}

	// Primitive controls are served as assets so a tool's own UI can load the
	// same element SRCOS's generated form uses.
	mux.HandleFunc("/assets/", s.handleAsset)

	// Tool catalogue and the generated fallback form (ADR-017).
	mux.HandleFunc("/tools", s.handleToolsPage)
	mux.HandleFunc("/tools/", s.handleToolFormPage)

	// The management console (admins only; non-admins are redirected, not shown
	// a bare 403, because the page is not a secret).
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/admin/", s.handleAdminSubPage)

	// The built frontend (webui/, Vite): only the canvas page loads it, and it
	// is embedded in this binary (ADR-012).
	mux.HandleFunc("/ui/", s.handleUI)

	// Root route (dashboard/login page)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			// Bare service paths (e.g. /jupyter) must be redirected to the proxied
			// URL before route-cookie forwarding can hijack them and 404.
			session := s.sessionFromCookies(r.Header.Get("Cookie"))
			if session.Valid && session.UserID != "" && r.Method == "GET" {
				if s.redirectBareService(w, r, session.UserID) {
					return
				}
			}
			// Page-like bare paths go deterministically to the user's default
			// service (root-routed SPAs that cannot understand a /proxy/ prefix);
			// resource-like paths (assets, /api calls) follow the Referer of the
			// page that issued them, then the route cookie, then the default
			// service as a last resort.
			if s.handleDefaultServicePage(w, r) {
				return
			}
			if s.handleRefererProxy(w, r) {
				return
			}
			if s.handleRouteCookieProxy(w, r) {
				return
			}
			if s.handleDefaultServiceProxy(w, r) {
				return
			}
			sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
			return
		}
		// GET / is the SRCOS dashboard. Only a clear backend referer may route it.
		// Cookie redirect must run BEFORE handleRefererProxy so the browser
		// address bar is updated to the real proxy URL (not stuck at /?xxx).
		if s.handleProxyEscape(w, r) {
			return
		}
		if s.handleRefererProxy(w, r) {
			return
		}
		s.handleRoot(w, r)
	})

	// Wrap with panic recovery
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// net/http aborts handlers with ErrAbortHandler when the client
				// disconnects mid-response (httputil.ReverseProxy panics with it
				// on a copy error while streaming, e.g. SSE). The connection is
				// already dead: swallowing the panic is correct, and logging a
				// stack trace or writing an error would only be noise.
				if rec == http.ErrAbortHandler {
					return
				}
				log.Printf("[srcos] panic: %v\n%s", rec, debug.Stack())
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		mux.ServeHTTP(w, r)
	})
}

// handleBackendLogin detects POST /login from a proxied backend (e.g. Jupyter login form).
// If the user already has a valid session and the Referer or route cookie points to a
// backend service, the request is forwarded there instead of being handled by srcos.
func (s *Server) handleBackendLogin(w http.ResponseWriter, r *http.Request) bool {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		return false
	}

	// Try to find target service from referer or route cookie
	match := s.findBackendService(r, session.UserID)
	if match == nil {
		return false
	}

	// Improved detection: check if this is a SRCOS login vs backend login
	ct := r.Header.Get("Content-Type")
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")

	// SRCOS login characteristics:
	// 1. Path is exactly /login (not /proxy/.../login)
	// 2. Origin/Referer doesn't contain /proxy/ path
	// 3. Content-Type is form data
	isSRCOSLogin := r.URL.Path == "/login" &&
		strings.Contains(ct, "application/x-www-form-urlencoded") &&
		!strings.Contains(origin, "/proxy/") &&
		!strings.Contains(referer, "/proxy/")

	if isSRCOSLogin {
		// Let SRCOS handle this login
		return false
	}

	// This appears to be a backend service login form, forward it
	s.forwardAsIs(w, r, match)
	return true
}

// findBackendService tries to locate the target service from Referer or route cookie.
func (s *Server) findBackendService(r *http.Request, username string) *config.ServiceMatch {
	// Referer is the strongest signal for backend-originated login posts.
	referer := r.Header.Get("Referer")
	if referer != "" {
		refPath := "/"
		if u, err := url.Parse(referer); err == nil && u.Path != "" {
			refPath = u.Path
		}
		if match := s.matchRouteForUser(refPath, username); match != nil {
			return match
		}
	}

	route := auth.GetRouteCookieForRequest(r.Header.Get("Cookie"), r.URL.Path, referer)
	if route == "" {
		return nil
	}
	return s.matchRouteForUser(route, username)
}

func (s *Server) handleProxyEscape(w http.ResponseWriter, r *http.Request) bool {
	// When a user lands on /?xxx (e.g. /?refresh=1) after escaping the proxy
	// prefix, redirect them back to the correct proxy URL via the route cookie.
	if r.URL.RawQuery == "" {
		return false
	}
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		return false
	}
	route := auth.GetRouteCookieForRequest(
		r.Header.Get("Cookie"), "", r.Header.Get("Referer"),
	)
	if route == "" || !strings.HasPrefix(route, "/proxy/"+session.UserID+"/") {
		return false
	}
	target := route
	if !strings.HasSuffix(target, "/") {
		target += "/"
	}
	target += "?" + r.URL.RawQuery
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

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

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !validLoginNext(next) {
		next = ""
	}
	sendHTML(w, 200, web.LoginPage(s.siteTitle, "", next))
}

// validLoginNext restricts post-login redirects to in-site proxy URLs,
// preventing open-redirect abuse.
func validLoginNext(next string) bool {
	return strings.HasPrefix(next, "/proxy/") && !strings.HasPrefix(next, "//")
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Parse the form before rate limiting: the limit key combines the client
	// IP with the submitted username so a shared-NAT office/lab does not lock
	// out everyone on one user's typos, and an attacker cannot freeze a whole
	// subnet by flooding failures.
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.LoginPage(s.siteTitle, "登录请求处理失败", ""))
		return
	}

	ip := auth.ClientIP(r)
	username := strings.TrimSpace(r.FormValue("username"))
	// Username matching is case-sensitive, but rate limiting deliberately is
	// not, so "Alice" and "alice" share a failure budget.
	rateKey := ip + "|" + strings.ToLower(username)

	if s.loginLimiter.IsBlocked(rateKey) {
		sendHTML(w, 429, web.LoginPage(s.siteTitle, "登录尝试过多，请 15 分钟后再试", ""))
		return
	}

	password := r.FormValue("password")
	next := r.FormValue("next")
	if !validLoginNext(next) {
		next = ""
	}

	if !config.IsValidUsername(username) || password == "" {
		s.loginLimiter.RecordFailure(rateKey)
		sendHTML(w, 401, web.LoginPage(s.siteTitle, loginErrorMsg, next))
		return
	}

	userConfig := s.registry.GetUserConfigForLogin(username)
	passwordHash := ""
	if userConfig != nil {
		passwordHash = userConfig.Auth.PasswordHash
	}
	// Timing-safe verify: even for unknown usernames a bcrypt comparison runs,
	// so login latency cannot be used to enumerate valid accounts.
	if auth.VerifyPasswordTimingSafe(password, passwordHash) {
		s.loginLimiter.Reset(rateKey)
		secure := auth.IsSecureRequest(r)
		if next == "" {
			next = "/"
		}
		// Account has TOTP enabled: hold at the second factor before issuing
		// a real session.
		if userConfig != nil && userConfig.Auth.TOTPSecret != "" {
			w.Header().Set("Set-Cookie", auth.SetPendingCookie(s.sessionSecret, pendingTTLSeconds, username, secure))
			http.Redirect(w, r, "/login/2fa?next="+url.QueryEscape(next), http.StatusFound)
			return
		}
		cookie := s.issueSessionCookie(username, secure)
		w.Header().Set("Set-Cookie", cookie)
		http.Redirect(w, r, next, http.StatusFound)
		return
	}

	s.loginLimiter.RecordFailure(rateKey)
	sendHTML(w, 401, web.LoginPage(s.siteTitle, loginErrorMsg, next))
}

// ---- two-factor authentication (TOTP) ----

// handleTwoFA serves the second login step: GET renders the code form (only
// when a valid pending token exists); POST verifies the code.
func (s *Server) handleTwoFA(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		if !auth.SameOriginRequest(r) {
			sendHTML(w, 403, web.TwoFAPage(s.siteTitle, "请求被拒绝", ""))
			return
		}
		s.handleTwoFAPost(w, r)
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	userID, ok := auth.ValidatePendingToken(cookies[auth.TOTACookieName], s.sessionSecret)
	if !ok || userID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	next := r.URL.Query().Get("next")
	if !validLoginNext(next) {
		next = ""
	}
	sendHTML(w, 200, web.TwoFAPage(s.siteTitle, "", next))
}

func (s *Server) handleTwoFAPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.TwoFAPage(s.siteTitle, "请求处理失败", ""))
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	userID, ok := auth.ValidatePendingToken(cookies[auth.TOTACookieName], s.sessionSecret)
	if !ok || userID == "" {
		// Pending token missing/expired: back to the password step.
		next := r.FormValue("next")
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}

	ip := auth.ClientIP(r)
	rateKey := ip + "|2fa|" + strings.ToLower(userID)
	if s.totpLimiter.IsBlocked(rateKey) {
		sendHTML(w, 429, web.TwoFAPage(s.siteTitle, "验证尝试过多，请 15 分钟后再试", r.FormValue("next")))
		return
	}

	uc := s.registry.GetUserConfigForLogin(userID)
	if uc == nil || uc.Auth.TOTPSecret == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTP(uc.Auth.TOTPSecret, code, time.Now()) {
		s.totpLimiter.RecordFailure(rateKey)
		sendHTML(w, 401, web.TwoFAPage(s.siteTitle, "动态码错误或已过期", r.FormValue("next")))
		return
	}

	s.totpLimiter.Reset(rateKey)
	secure := auth.IsSecureRequest(r)
	w.Header().Set("Set-Cookie", s.issueSessionCookie(userID, secure))
	w.Header().Add("Set-Cookie", auth.ClearPendingCookie())
	next := r.FormValue("next")
	if !validLoginNext(next) {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// handleTwoFASetup enrolls (or rotates) TOTP for the logged-in user.
func (s *Server) handleTwoFASetup(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.Method == "POST" {
		if !auth.SameOriginRequest(r) {
			sendHTML(w, 403, web.TwoFASetupPage(s.siteTitle, "请求被拒绝", "", s.twoFAEnabled(session.UserID)))
			return
		}
		s.handleTwoFASetupPost(w, r, session.UserID)
		return
	}

	enabled := s.twoFAEnabled(session.UserID)
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "生成密钥失败", "", enabled))
		return
	}
	secure := auth.IsSecureRequest(r)
	w.Header().Set("Set-Cookie", auth.SetSetupCookie(secret, s.sessionSecret, pendingTTLSeconds, secure))
	sendHTML(w, 200, web.TwoFASetupPage(s.siteTitle, "", secret, enabled))
}

func (s *Server) handleTwoFASetupPost(w http.ResponseWriter, r *http.Request, username string) {
	enabled := s.twoFAEnabled(username)
	if err := r.ParseForm(); err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "请求处理失败", "", enabled))
		return
	}

	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	secret, ok := auth.ValidateSetupToken(cookies[auth.TOTASetupCookie], s.sessionSecret)
	if !ok {
		sendHTML(w, 401, web.TwoFASetupPage(s.siteTitle, "设置会话已过期，请重新开始", "", enabled))
		return
	}

	code := strings.TrimSpace(r.FormValue("code"))
	if !auth.VerifyTOTP(secret, code, time.Now()) {
		sendHTML(w, 401, web.TwoFASetupPage(s.siteTitle, "动态码错误或已过期，请重试", secret, enabled))
		return
	}

	user := s.registry.GetUser(username)
	if user == nil {
		sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
		return
	}
	if err := config.UpdateTOTPSecret(user.ConfigPath, secret); err != nil {
		sendHTML(w, 500, web.TwoFASetupPage(s.siteTitle, "保存失败：配置文件不可写", secret, enabled))
		return
	}
	s.registry.Reload()
	w.Header().Add("Set-Cookie", auth.ClearSetupCookie())
	http.Redirect(w, r, "/", http.StatusFound)
}

// twoFAEnabled reports whether the user has a TOTP secret configured.
func (s *Server) twoFAEnabled(username string) bool {
	user := s.registry.GetUser(username)
	return user != nil && user.Config.Auth.TOTPSecret != ""
}

// handleTwoFAQr renders the provisioning QR code for the pending setup secret.
func (s *Server) handleTwoFAQr(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	cookies := auth.ParseCookies(r.Header.Get("Cookie"))
	secret, ok := auth.ValidateSetupToken(cookies[auth.TOTASetupCookie], s.sessionSecret)
	if !ok {
		http.Error(w, "invalid setup", http.StatusBadRequest)
		return
	}
	png, err := qrcode.Encode(auth.TOTPURI(session.UserID, secret), qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "qr error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

// handleTwoFADisable turns off TOTP for the logged-in user.
func (s *Server) handleTwoFADisable(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	if r.Method != "POST" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if !auth.SameOriginRequest(r) {
		sendHTML(w, 403, web.NotFoundPage(s.siteTitle))
		return
	}

	user := s.registry.GetUser(session.UserID)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := config.UpdateTOTPSecret(user.ConfigPath, ""); err != nil {
		sendHTML(w, 500, web.LoginPage(s.siteTitle, "关闭失败：配置文件不可写", ""))
		return
	}
	s.registry.Reload()
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		// Send the unauthenticated user to the login page and remember where
		// they were headed so they land back on their service after login.
		next := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
		return
	}

	// Refresh session if needed
	if auth.ShouldRefreshSession(session, s.state.Auth.SessionTTL) {
		secure := auth.IsSecureRequest(r)
		cookie := s.issueSessionCookie(session.UserID, secure)
		w.Header().Add("Set-Cookie", cookie)
	}

	path := r.URL.Path

	// Instance-backed services first: a live instance beats a static card,
	// because the card is a hand-written pointer that may be stale (package
	// route documents the same precedence).
	if match := s.instanceMatch(path, session.UserID); match != nil {
		s.proxyRequest(w, r, match)
		return
	}

	// Try multi-user path: /proxy/{user}/{service}/...
	multiMatch := s.registry.FindService(path)
	if multiMatch != nil {
		pathUser := config.UsernameFromProxyPath(path)
		if pathUser == "" || pathUser != session.UserID {
			sendHTML(w, 403, web.NotFoundPage(s.siteTitle))
			return
		}
		s.proxyRequest(w, r, multiMatch)
		return
	}

	// Try legacy path
	legacyMatch := s.registry.FindLegacyService(path, session.UserID)
	if legacyMatch == nil {
		// Nothing can serve this path. Before saying 404, explain what happened
		// to the instance the path names: a progress page while it starts, the
		// reason once it died. It runs last so a stopped instance never shadows
		// a working static card.
		if toolID := toolFromProxyPath(path, session.UserID); toolID != "" &&
			s.serveInstanceStatus(w, r, session.UserID, toolID) {
			return
		}
		sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
		return
	}

	s.proxyRequest(w, r, legacyMatch)
}

// proxyRequest forwards a direct /proxy/{user}/{service}/... request, rewriting the path.
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, match *config.ServiceMatch) {
	// Check WebSocket upgrade requests
	if isWebSocketUpgrade(r) && !match.Service.WebSocket {
		http.Error(w, "WebSocket not enabled for this service", http.StatusForbidden)
		return
	}

	forwardCtx := proxy.BuildProxyForwardContext(r, match.Username, match.Service.Path, match.Legacy)
	forwardCtx.SSO = s.state.SSO
	// The injected <base> must point at the directory of the actual document
	// (e.g. /proxy/user/svc/report/page.html -> /proxy/user/svc/report/), not
	// the service prefix, so relative links in plain HTML files in
	// subdirectories resolve correctly.
	forwardCtx.Base = proxy.BaseForPath(forwardCtx.Prefix, match.RemainingPath)
	for _, c := range auth.SetRouteCookies(forwardCtx.Prefix) {
		w.Header().Add("Set-Cookie", c)
	}
	r.URL.Path = match.RemainingPath
	s.forwardToBackend(w, r, match.Service, forwardCtx)
}

// forwardAsIs forwards a request to the backend keeping the current request path intact.
// Used for referer-based, route-cookie-based, and backend-login routing.
func (s *Server) forwardAsIs(w http.ResponseWriter, r *http.Request, match *config.ServiceMatch) {
	// Check WebSocket upgrade requests
	if isWebSocketUpgrade(r) && !match.Service.WebSocket {
		http.Error(w, "WebSocket not enabled for this service", http.StatusForbidden)
		return
	}

	forwardCtx := proxy.BuildProxyForwardContext(r, match.Username, match.Service.Path, match.Legacy)
	forwardCtx.SSO = s.state.SSO
	for _, c := range auth.SetRouteCookies(forwardCtx.Prefix) {
		w.Header().Add("Set-Cookie", c)
	}
	s.forwardToBackend(w, r, match.Service, forwardCtx)
}

func (s *Server) forwardToBackend(w http.ResponseWriter, r *http.Request, svc *config.ServiceConfig, fc proxy.ProxyForwardContext) {
	// Record the use before it happens: an instance is "in use" from the moment
	// someone asks for it, and a request that fails is still evidence that the
	// service is wanted.
	instanceID := s.touchActivity(fc.Username, svc)
	if isWebSocketUpgrade(r) {
		// A tunnel that stays open is activity in its own right, counted until
		// the connection closes (the reaper asks before reclaiming).
		w = proxy.TrackHijack(w, instanceID, s.activeConns)
	}

	rp := proxy.NewReverseProxy(svc, fc)
	rp.Transport = s.transport
	// Go's ReverseProxy logs body-copy errors ("unexpected EOF") through its
	// internal logger whenever the client disconnects mid-stream (SSE tab
	// close, navigation). Those are expected, not outages, and meaningful
	// failures are already logged by ErrorHandler below, so silence the
	// standard-library noise.
	rp.ErrorLog = log.New(io.Discard, "", 0)
	// Add error handler: log the full error server-side, reply generically so
	// backend host/port details are not leaked to clients.
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// The client went away (tab closed / navigation during a stream); the
		// connection is dead, so there is nothing to write and the "context
		// canceled" error is expected, not an outage.
		if r.Context().Err() != nil {
			return
		}
		log.Printf("[srcos] proxy error: service=%s, path=%s, error=%v", svc.ID, r.URL.Path, err)
		switch {
		case strings.Contains(err.Error(), "connection refused"):
			// The instance may have been stopped by another process (the CLI);
			// nothing tells this one except the record, so re-check before the
			// next request rather than dialing a dead port for a whole tick.
			s.dropDeadRoute(fc.Username, svc)
			http.Error(w, "Service unavailable: backend not responding", http.StatusBadGateway)
		case strings.Contains(err.Error(), "timeout"):
			http.Error(w, "Service timeout: backend took too long to respond", http.StatusGatewayTimeout)
		default:
			http.Error(w, "Proxy error", http.StatusBadGateway)
		}
	}
	rp.ServeHTTP(w, r)
}

func (s *Server) handleRefererProxy(w http.ResponseWriter, r *http.Request) bool {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		return false
	}

	// Refresh session if needed
	if auth.ShouldRefreshSession(session, s.state.Auth.SessionTTL) {
		secure := auth.IsSecureRequest(r)
		cookie := s.issueSessionCookie(session.UserID, secure)
		w.Header().Add("Set-Cookie", cookie)
	}

	referer := r.Header.Get("Referer")
	if referer == "" {
		return false
	}

	// Extract path from referer
	refPath := "/"
	if u, err := url.Parse(referer); err == nil && u.Path != "" {
		refPath = u.Path
	}

	match := s.matchRouteForUser(refPath, session.UserID)
	if match == nil {
		return false
	}

	s.forwardAsIs(w, r, match)
	return true
}

func (s *Server) handleRouteCookieProxy(w http.ResponseWriter, r *http.Request) bool {
	route := auth.GetRouteCookieForRequest(r.Header.Get("Cookie"), r.URL.Path, r.Header.Get("Referer"))
	if route == "" {
		return false
	}

	// Route cookies are context hints, not credentials: without a valid
	// session they must never grant backend access (the session may have
	// expired or the user may have logged out).
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		next := r.URL.RequestURI()
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusFound)
		return true
	}

	// Refresh session if needed
	if auth.ShouldRefreshSession(session, s.state.Auth.SessionTTL) {
		secure := auth.IsSecureRequest(r)
		cookie := s.issueSessionCookie(session.UserID, secure)
		w.Header().Add("Set-Cookie", cookie)
	}

	match := s.matchRouteForUser(route, session.UserID)
	if match == nil {
		return false
	}

	s.forwardAsIs(w, r, match)
	return true
}

// handleDefaultServiceProxy forwards unclaimed bare gateway paths to the
// user's configured default service (DefaultService), so root-routed SPAs
// (Nuxt / Next / Vite client routers, which match window.location.pathname
// against root routes and cannot understand a /proxy/<user>/ prefix) work at
// the gateway root with zero app-side basePath/baseURL changes. It runs only
// after route-cookie and Referer forwarding have failed; forwardAsIs stamps
// the route cookies that keep subsequent sub-resource requests on the same
// service. Without a valid session the request falls through to the 404.
func (s *Server) handleDefaultServiceProxy(w http.ResponseWriter, r *http.Request) bool {
	return s.forwardToDefaultService(w, r)
}

// handleDefaultServicePage deterministically routes page-like bare paths
// (SPA views like /dashboard, not assets or API calls) to the user's default
// service, before Referer / route-cookie forwarding. This makes root-based
// SPA navigation stable regardless of which other services were visited most
// recently; resource-like paths are left to the Referer/route-cookie chain so
// they stick to the page that issued them.
func (s *Server) handleDefaultServicePage(w http.ResponseWriter, r *http.Request) bool {
	if !barePathIsPage(r.URL.Path) {
		return false
	}
	return s.forwardToDefaultService(w, r)
}

func (s *Server) forwardToDefaultService(w http.ResponseWriter, r *http.Request) bool {
	session := s.sessionFromCookies(r.Header.Get("Cookie"))
	if !session.Valid || session.UserID == "" {
		return false
	}
	user := s.registry.GetUser(session.UserID)
	if user == nil || user.Config.DefaultService == "" {
		return false
	}
	svc := config.ServiceByID(user.Config.Services, user.Config.DefaultService)
	if svc == nil {
		return false
	}
	match := &config.ServiceMatch{
		Username:      session.UserID,
		Service:       svc,
		RemainingPath: r.URL.Path,
	}
	s.forwardAsIs(w, r, match)
	return true
}

// barePathIsPage reports whether an unclaimed bare gateway path looks like a
// page navigation (a client-routed SPA view) rather than a static asset or
// API call. Page-like paths are routed to the user's default service
// deterministically; resource-like paths (segments with file extensions, or
// paths under known asset/API roots) fall back to Referer / route-cookie
// forwarding so they follow the page that requested them.
func barePathIsPage(p string) bool {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	lower := strings.ToLower(p)
	for _, seg := range strings.Split(lower, "/") {
		if i := strings.LastIndexByte(seg, '.'); i > 0 && i < len(seg)-1 {
			return false
		}
	}
	for _, root := range []string{
		"/api/", "/plugins/", "/_nuxt/", "/_next/", "/assets/", "/static/",
		"/vendors/", "/files/", "/favicon.", "/manifest.webmanifest",
	} {
		if strings.HasPrefix(lower, root) {
			return false
		}
	}
	return true
}

func (s *Server) redirectBareService(w http.ResponseWriter, r *http.Request, username string) bool {
	path := r.URL.Path
	query := ""
	if r.URL.RawQuery != "" {
		query = "?" + r.URL.RawQuery
	}

	// Service instances first: they are live, and the same short URL should work
	// for both kinds of service.
	if target, ok := s.barePathRoute(path, username); ok {
		http.Redirect(w, r, target+query, http.StatusFound)
		return true
	}

	user := s.registry.GetUser(username)
	if user == nil {
		return false
	}

	var bestSvc *config.ServiceConfig
	bestLen := -1
	for i := range user.Config.Services {
		svc := &user.Config.Services[i]
		if path == svc.Path || strings.HasPrefix(path, svc.Path+"/") {
			if len(svc.Path) > bestLen {
				bestLen = len(svc.Path)
				bestSvc = svc
			}
		}
	}
	if bestSvc == nil {
		return false
	}

	rest := path[len(bestSvc.Path):]
	http.Redirect(w, r, "/proxy/"+username+bestSvc.Path+rest+query, http.StatusFound)
	return true
}

// ScanLoop periodically reloads the user registry.
func (s *Server) ScanLoop(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			before := len(s.registry.ListUsers())
			s.registry.Reload()
			after := len(s.registry.ListUsers())
			// Only log when the registry actually changed to avoid log spam.
			if after != before {
				log.Printf("[srcos] user registry changed: %d -> %d user(s)", before, after)
			}
			// Keep the routing table in step with the instance records: a
			// service started or stopped by the CLI (another process) becomes
			// reachable or unreachable within one tick.
			s.syncRoutes()
			// A hand-edited grants.yaml takes effect here, without a restart.
			s.reloadPolicyIfChanged()
			// Enforce the lifecycle ceilings (idleTTL / maxLifetime). Idle
			// means "no traffic", which is why the reaper is given the proxy's
			// observations rather than the record's start time.
			s.reapServices()
		case <-stop:
			return
		}
	}
}

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

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	connection := strings.ToLower(r.Header.Get("Connection"))
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	return strings.Contains(connection, "upgrade") && upgrade == "websocket"
}

// ─────────────────────────────────────────────────────────────────────────
// 原语控件与工具页面
// ─────────────────────────────────────────────────────────────────────────

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

	sendHTML(w, 200, web.AdminPage(s.siteTitle, username, web.AdminData{
		Instances:    instances,
		Tools:        tools,
		Admins:       snapshot.Admins,
		Groups:       snapshot.Groups,
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
	http.NotFound(w, r)
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
	return mcp.NewServer(s.mcpVersion, s.reader(), s.agentTokens)
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
	sendHTML(w, 200, web.ToolsPage(s.siteTitle, username, tools))
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
		sendHTML(w, 200, web.ToolFormPage(s.siteTitle, username, t, s.storagesForTool(t)))
		return
	}
	sendHTML(w, 404, web.NotFoundPage(s.siteTitle))
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
	}
}

// visibleTools applies the grant filter through the shared read side.
func (s *Server) visibleTools(username string) ([]*tool.Tool, error) {
	return s.reader().VisibleManifests(username)
}

func (s *Server) storagesForTool(t *tool.Tool) []storage.Storage {
	return s.reader().StorageDefs(t)
}
