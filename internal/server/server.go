package server

import (
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/seqyuan/srcos/internal/activity"
	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/api"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/execute"
	"github.com/seqyuan/srcos/internal/grant"
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
	// taskQueue drains the submission drop-box: the daemon that makes a
	// submission run by itself. Nil when draining is disabled (or when the write
	// path is not configured at all).
	taskQueue *execute.Queue
	// audit is the structured record of who did what (internal/audit). Shared
	// with the API handler; the gateway's own decisions (reconcile, reaper) can
	// record into it as well.
	audit *audit.Recorder
}

// Options carries the seams the gateway needs beyond the user registry:
// where tool packages live, and which shared data is declared.
//
// Every field is optional, and the zero value is the honest "not wired" for
// each: an empty ToolsDir looks beside the binary, a nil Storages loads
// config/storages.yaml (a missing file means "no shared data"), a nil Grants
// loads config/grants.yaml (deny-by-default when it is missing or malformed),
// a nil Runner means this process only serves, and TaskWorkers == 0 uses the
// default while < 0 disables the queue. A deployment with none of them still
// works: the tool pages and /api/paths report that they are not configured
// rather than pretending.
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
	// Routes is the dynamic routing table. When Supervisor is set its own table
	// is used regardless (the proxy must read the table the supervisor writes);
	// pass one only for a deployment that has no supervisor.
	Routes *route.Table
	// Supervisor, when set, is the runner the gateway uses to re-adopt live
	// instances at startup and to reclaim expired ones on the scan tick. It
	// must not carry a user: reconciling and reaping act on records, not on
	// behalf of an actor (package runtime refuses to start units without one).
	Supervisor *runtime.Runner
	// TaskWorkers is how many queued runs the gateway keeps in flight across the
	// host. Zero uses execute.DefaultTaskWorkers; a negative value disables the
	// task queue entirely (the deployment drains the drop-box by hand or from
	// another process).
	TaskWorkers int
	// Audit is the shared audit recorder (ADR-024). The caller supplies it when
	// it also handed the same recorder to the supervisor Runner it passes in, so
	// the write path, the lifecycle decisions and the API all append to one
	// stream. Nil means "build one from configDir", which is what a test wants.
	Audit *audit.Recorder
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
	// The routing table is the one place the orchestration layer and the proxy
	// meet: the supervisor publishes endpoints into it and this proxy reads
	// them (ADR-003). They must be the *same* table — a proxy reading a table
	// nobody writes makes every service unreachable. Derive it from the runner
	// when the caller passed none, and say so if the caller passed a different
	// one, so the two cannot silently drift.
	routes := opts.Routes
	if opts.Supervisor != nil {
		if own := opts.Supervisor.Options().Routes; own != nil {
			if routes != nil && routes != own {
				log.Printf("[srcos] Options.Routes is not the supervisor's routing table; using the supervisor's (the one that gets written)")
			}
			routes = own
		}
	}
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

	// The structured audit stream (internal/audit). Share the caller's recorder
	// when it gave one (so it also reached the supervisor's Runner); otherwise
	// build one here. Either way there is exactly one per process.
	auditRec := opts.Audit
	if auditRec == nil {
		auditRec = audit.New(config.DataDir(configDir))
	}

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
		Audit:       auditRec,
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
		audit:           auditRec,
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
	// The task queue drains the submission drop-box. It shares the API handler's
	// write-path controller, so a queued run goes through exactly the same
	// validation, grants and quotas a submitted one does.
	if opts.TaskWorkers >= 0 && opts.ToolsDir != "" && srv.apiHandler.Executor() != nil {
		srv.taskQueue = execute.NewQueue(srv.apiHandler.Executor(), execute.QueueOptions{
			Workers: opts.TaskWorkers,
			Users: func() []string {
				records := registry.ListUsers()
				names := make([]string, 0, len(records))
				for _, u := range records {
					names = append(names, u.Username)
				}
				return names
			},
		})
	}

	// Bring the records and the routing table in line with reality before
	// serving: adopt the instances that are still alive (they were started by
	// the CLI, in another process), mark the dead ones stopped, and publish the
	// survivors so a gateway restart does not take every service offline.
	srv.reconcile()
	srv.reconcileTasks()
	return srv
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
	// Each of these is a reserved gateway path (README「保留路径」).
	for _, p := range []string{"/api/tools", "/api/tools/", "/api/paths", "/api/jobs", "/api/jobs/", "/api/flows/",
		"/api/tokens", "/api/tokens/",
		"/api/resources", "/api/resources/raw", "/api/resources/html",
		"/api/admin", "/api/admin/"} {
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
	// A user's own access requests and their outcome (B3).
	mux.HandleFunc("/requests", s.handleRequestsPage)
	// The built-in resource viewer (srcos:// + the first batch of viewers,
	// ADR-011/016). A page, not JSON, because it is the platform's own fallback
	// UI and must work with JavaScript disabled.
	mux.HandleFunc("/view", s.handleView)

	// The user's own instance list and one instance's log (the live tail comes
	// from /api/jobs/<id>/logs?follow=1).
	mux.HandleFunc("/tasks", s.handleTasksPage)
	mux.HandleFunc("/tasks/", s.handleTaskPage)

	// Self-service agent tokens (ADR-019): a user mints the credential their
	// agent / MCP client carries, instead of asking an administrator.
	mux.HandleFunc("/tokens", s.handleTokensPage)

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
