package server

import (
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

import (
	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/proxy"
	"github.com/seqyuan/srcos/internal/web"
)

// This file is the proxy layer's half of ADR-003: it reads the routing table the
// orchestration layer writes and turns a request path into a backend connection.
// Nothing here starts, stops or inspects a unit.

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

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	connection := strings.ToLower(r.Header.Get("Connection"))
	upgrade := strings.ToLower(r.Header.Get("Upgrade"))
	return strings.Contains(connection, "upgrade") && upgrade == "websocket"
}

// ─────────────────────────────────────────────────────────────────────────
// 原语控件与工具页面
// ─────────────────────────────────────────────────────────────────────────
