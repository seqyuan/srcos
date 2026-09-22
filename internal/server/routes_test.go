package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// proxySink is a backend that records what it was asked for.
type proxySink struct {
	srv   *httptest.Server
	paths []string
	last  *http.Request
}

func newProxySink(t *testing.T) *proxySink {
	t.Helper()
	s := &proxySink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.URL.Path)
		s.last = r.Clone(r.Context())
		io.WriteString(w, "backend:"+r.URL.Path)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *proxySink) endpoint() string { return strings.TrimPrefix(s.srv.URL, "http://") }

// proxyGateway builds a gateway with two users and no static cards.
func proxyGateway(t *testing.T) (*Server, string) {
	t.Helper()
	configDir := t.TempDir()
	if err := os.MkdirAll(config.UsersDir(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"alice", "bob"} {
		cfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
		if err := os.WriteFile(config.UserConfigPath(configDir, user), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(&config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 30152},
		Auth:   config.AuthState{SessionSecret: "testsecret", SessionTTL: 86400},
	}, configDir)
	return srv, configDir
}

// saveService writes a service instance record the way `svc start` would.
func saveService(t *testing.T, configDir, user, toolID, endpoint string, mutate func(*runtime.Instance)) *runtime.Instance {
	t.Helper()
	paths := runtime.PathsFor(configDir, user, toolID, "")
	if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID:        runtime.InstanceID(user, toolID, ""),
		User:      user,
		Tool:      toolID,
		Kind:      string(tool.KindService),
		JobName:   toolID,
		State:     runtime.StateRunning,
		Backend:   "local",
		Sandbox:   "none",
		Endpoint:  endpoint,
		RoutePath: "/proxy/" + user + "/" + toolID,
		StartedAt: time.Now().UTC(),
		LogPath:   paths.LogPath,
	}
	if mutate != nil {
		mutate(inst)
	}
	if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
		t.Fatal(err)
	}
	return inst
}

func sessionCookie(user string) string {
	return auth.SetSessionCookie("testsecret", 86400, user, auth.SessionRev(strings.Repeat("1", 64)), false)
}

// get performs a request with a session for user and returns the response.
func (s *Server) getAs(t *testing.T, user, path, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	} else {
		req.Header.Set("Cookie", sessionCookie(user))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// getAsBrowser is getAs for a browser navigation: it asks for HTML, which is
// what makes the gateway answer with a page instead of a status line.
func (s *Server) getAsBrowser(t *testing.T, user, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Cookie", sessionCookie(user))
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// ─────────────────────────────────────────────────────────────────────────
// 动态路由：实例成为可达
// ─────────────────────────────────────────────────────────────────────────

func TestInstanceServiceIsReachableThroughTheGateway(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	rec := srv.getAs(t, "alice", "/proxy/alice/web/", "")
	if rec.Code != 200 {
		t.Fatalf("root: %d %s", rec.Code, rec.Body)
	}
	if rec.Body.String() != "backend:/" {
		t.Fatalf("the service prefix must be stripped, got %q", rec.Body)
	}

	// Sub-paths keep their remainder.
	rec = srv.getAs(t, "alice", "/proxy/alice/web/deep/page.html", "")
	if rec.Code != 200 || rec.Body.String() != "backend:/deep/page.html" {
		t.Fatalf("sub-path: %d %q", rec.Code, rec.Body)
	}
	// No trailing slash at the root works too.
	rec = srv.getAs(t, "alice", "/proxy/alice/web", "")
	if rec.Code != 200 || rec.Body.String() != "backend:/" {
		t.Fatalf("bare prefix: %d %q", rec.Code, rec.Body)
	}
}

// The CLI starts services in another process, so the gateway cannot know about
// a new one until it re-reads the records. A lookup that misses does exactly
// that, which is what makes "start it and click the link" work.
func TestAServiceStartedAfterTheGatewayIsReachable(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)

	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code == 200 {
		t.Fatal("nothing is running yet")
	}
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)

	// No restart, no sync: the cache-miss path must find it.
	rec := srv.getAs(t, "alice", "/proxy/alice/web/", "")
	if rec.Code != 200 {
		t.Fatalf("a freshly started service must be reachable: %d %s", rec.Code, rec.Body)
	}
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("and stay reachable (cached): %d", rec.Code)
	}
}

func TestStoppedServiceStopsBeingReachable(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}

	// The stop happened in another process: all this one sees is the record.
	saveService(t, configDir, "alice", "web", sink.endpoint(), func(i *runtime.Instance) {
		i.State = runtime.StateStopped
	})
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code == 200 {
		t.Fatalf("a stopped instance must not be routed: %d", rec.Code)
	}

	// A service that is still starting is not routed either: there is nothing
	// to dial yet.
	saveService(t, configDir, "alice", "web", sink.endpoint(), func(i *runtime.Instance) {
		i.State = runtime.StateStarting
	})
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code == 200 {
		t.Fatalf("a starting instance must not be routed: %d", rec.Code)
	}

	// Idle is still serving (a service with no recent traffic).
	saveService(t, configDir, "alice", "web", sink.endpoint(), func(i *runtime.Instance) {
		i.State = runtime.StateIdle
	})
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("an idle instance is still serving: %d", rec.Code)
	}
}

func TestDeletedRecordDropsTheRoute(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	inst := saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}

	if err := runtime.DeleteInstance(runtime.InstancePath(configDir, inst.ID)); err != nil {
		t.Fatal(err)
	}
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code == 200 {
		t.Fatal("a route whose record is gone must be dropped: a stale entry keeps sending traffic to a port the OS may reuse")
	}
	if srv.routes.Len() != 0 {
		t.Fatalf("table still holds %d entries", srv.routes.Len())
	}
}

func TestInstanceRoutesAreScopedToTheirUser(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "bob", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	// Alice must not reach bob's service, even though it is running and the
	// path is well-formed.
	rec := srv.getAs(t, "alice", "/proxy/bob/web/", "")
	if rec.Code == 200 {
		t.Fatalf("cross-user access was allowed: %q", rec.Body)
	}
	// Bob can.
	if rec := srv.getAs(t, "bob", "/proxy/bob/web/", ""); rec.Code != 200 {
		t.Fatalf("the owner must reach it: %d %s", rec.Code, rec.Body)
	}
}

// A record is a file on disk. A hand-edited one must not turn the gateway into
// an SSRF proxy by naming a public address or a metadata endpoint.
func TestNonLoopbackEndpointIsNotRouted(t *testing.T) {
	srv, configDir := proxyGateway(t)

	for _, endpoint := range []string{"10.0.0.5:8080", "169.254.169.254:80", "example.com:80", "not-an-endpoint"} {
		saveService(t, configDir, "alice", "web", endpoint, nil)
		srv.syncRoutes()
		if _, ok := srv.routes.Get("alice", "web"); ok {
			t.Fatalf("endpoint %q must not become a route", endpoint)
		}
		if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code == 200 {
			t.Fatalf("endpoint %q was routed: %d", endpoint, rec.Code)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 裸路径（SPA / Referer）
// ─────────────────────────────────────────────────────────────────────────

// A service instance that is an SPA generates absolute paths (/api/...), which
// the browser sends to the gateway root. The route cookie set by the first
// /proxy/... request has to bring them back to the instance.
func TestBarePathFollowsTheRouteCookieToAnInstance(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	rec := srv.getAs(t, "alice", "/proxy/alice/web/", "")
	if rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	cookies := rec.Header().Values("Set-Cookie")
	if len(cookies) == 0 {
		t.Fatal("the proxy must set a route cookie")
	}

	// One Cookie header, the way a browser sends them: the server reads the
	// first value only, so two headers would look like "no session".
	jar := []string{}
	for _, c := range cookies {
		jar = append(jar, strings.SplitN(c, ";", 2)[0])
	}
	jar = append(jar, sessionCookie("alice"))
	req := httptest.NewRequest("GET", "/api/thing", nil)
	req.Header.Set("Cookie", strings.Join(jar, "; "))
	got := httptest.NewRecorder()
	srv.Handler().ServeHTTP(got, req)
	if got.Code != 200 || got.Body.String() != "backend:/api/thing" {
		t.Fatalf("bare path with a route cookie: %d %q", got.Code, got.Body)
	}
	// A bare path is forwarded as-is: the backend's own router sees /api/thing.
	if sink.paths[len(sink.paths)-1] != "/api/thing" {
		t.Fatalf("backend saw %q", sink.paths[len(sink.paths)-1])
	}
}

func TestRefererBringsBarePathsBackToAnInstance(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	req := httptest.NewRequest("GET", "/static/app.js", nil)
	req.Header.Set("Cookie", sessionCookie("alice"))
	req.Header.Set("Referer", "http://gw:30152/proxy/alice/web/dashboard")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.String() != "backend:/static/app.js" {
		t.Fatalf("referer forwarding: %d %q", rec.Code, rec.Body)
	}
}

// A WebSocket upgrade reaches an instance (a notebook or terminal in the
// browser is the normal case), even though the backend here is not a WS server:
// what matters is that the proxy did not refuse it as "not enabled".
func TestWebSocketUpgradeIsForwardedToAnInstance(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	req := httptest.NewRequest("GET", "/proxy/alice/web/ws", nil)
	req.Header.Set("Cookie", sessionCookie("alice"))
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatal("an instance route must be published with WebSocket support")
	}
	if len(sink.paths) == 0 || sink.paths[len(sink.paths)-1] != "/ws" {
		t.Fatalf("the upgrade did not reach the backend: %v", sink.paths)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 与静态卡片的关系
// ─────────────────────────────────────────────────────────────────────────

// writeCard writes a static card into a user's config, the way the dashboard or
// `/api/services` would.
func writeCard(t *testing.T, configDir, servicePath, endpoint string, websocket bool) {
	t.Helper()
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		t.Fatal(err)
	}
	cfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\n" +
		"services:\n  - id: web\n    name: Web\n    host: " + host + "\n    port: " + port + "\n" +
		"    path: " + servicePath + "\n    websocket: " + boolText(websocket) + "\n"
	if err := os.WriteFile(config.UserConfigPath(configDir, "alice"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cardGateway adds a static card for alice pointing at a sink.
func cardGateway(t *testing.T, sink *proxySink, servicePath string, websocket bool) (*Server, string) {
	t.Helper()
	srv, configDir := proxyGateway(t)
	writeCard(t, configDir, servicePath, sink.endpoint(), websocket)
	srv.registry.Reload()
	return srv, configDir
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestStaticCardsStillWork(t *testing.T) {
	sink := newProxySink(t)
	srv, _ := cardGateway(t, sink, "/jupyter", true)

	rec := srv.getAs(t, "alice", "/proxy/alice/jupyter/lab", "")
	if rec.Code != 200 || rec.Body.String() != "backend:/lab" {
		t.Fatalf("card proxying: %d %q", rec.Code, rec.Body)
	}
}

// An instance is live and a card is a hand-written pointer, so the instance
// wins when both exist for the same path (package route documents the same
// precedence).
func TestALiveInstanceBeatsAStaleCard(t *testing.T) {
	cardSink := newProxySink(t)
	srv, configDir := cardGateway(t, cardSink, "/web", true)

	instSink := newProxySink(t)
	saveService(t, configDir, "alice", "web", instSink.endpoint(), nil)
	srv.syncRoutes()

	rec := srv.getAs(t, "alice", "/proxy/alice/web/", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), instSink.endpoint()) && rec.Body.String() != "backend:/" {
		t.Fatalf("unexpected body %q", rec.Body)
	}
	if len(instSink.paths) != 1 || len(cardSink.paths) != 0 {
		t.Fatalf("the instance must win: instance paths=%v card paths=%v", instSink.paths, cardSink.paths)
	}

	// And when the instance stops, the card is what is left.
	saveService(t, configDir, "alice", "web", instSink.endpoint(), func(i *runtime.Instance) {
		i.State = runtime.StateStopped
	})
	srv.syncRoutes()
	rec = srv.getAs(t, "alice", "/proxy/alice/web/x", "")
	if rec.Code != 200 || rec.Body.String() != "backend:/x" {
		t.Fatalf("fallback to the card: %d %q", rec.Code, rec.Body)
	}
	if len(cardSink.paths) != 1 {
		t.Fatalf("card paths = %v", cardSink.paths)
	}
}

// A service stopped by another process leaves a stale entry in this one's
// table. The first request after that gets an honest 502, and the entry is
// dropped so every later request asks the record instead — which says the
// service is gone.
func TestADeadRouteIsDroppedAfterAFailedDial(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}

	// The process dies and the record is updated by whoever stopped it.
	sink.srv.Close()
	saveService(t, configDir, "alice", "web", sink.endpoint(), func(i *runtime.Instance) {
		i.State = runtime.StateStopped
	})

	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("the first request to a dead target = %d, want 502", rec.Code)
	}
	if _, ok := srv.routes.Get("alice", "web"); ok {
		t.Fatal("the dead route must be dropped, not dialed again")
	}
	// From then on the record is the answer: not running, not routed — and,
	// since no card can serve the path either, the gateway explains the state
	// instead of dialing anything again.
	second := srv.getAsBrowser(t, "alice", "/proxy/alice/web/")
	if strings.Contains(second.Body.String(), "backend not responding") {
		t.Fatalf("the gateway dialed the dropped route again: %s", second.Body)
	}
	if second.Code != http.StatusBadGateway || !strings.Contains(second.Body.String(), "服务未在运行") {
		t.Fatalf("expected the status page, got %d %s", second.Code, second.Body)
	}
}

// A service that is merely restarting is republished from its record (which
// still says running), so dropping the entry on a failed dial is not a
// one-way door.
func TestADroppedRouteComesBackIfTheRecordStillSaysRunning(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()
	srv.dropDeadRoute("alice", &config.ServiceConfig{ID: "web", Host: "127.0.0.1", Port: numberFromEndpoint(t, sink.endpoint())})

	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("a still-running instance must be republished: %d %s", rec.Code, rec.Body)
	}
}

func numberFromEndpoint(t *testing.T, endpoint string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// A bare short URL (/websvc/...) redirects to the instance's proxied path, the
// same way a static card's bare path does: the user should not have to know
// which kind of service they are looking at.
func TestBareShortPathRedirectsToAnInstance(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	rec := srv.getAs(t, "alice", "/web/lab?x=1", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != "/proxy/alice/web/lab?x=1" {
		t.Fatalf("location = %q", loc)
	}
	// The redirect target must actually serve.
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/lab", ""); rec.Code != 200 {
		t.Fatalf("follow-up: %d", rec.Code)
	}
	// An unknown bare path is not redirected anywhere (no open redirect).
	if rec := srv.getAs(t, "alice", "/nope/x", ""); rec.Code == http.StatusFound {
		t.Fatalf("unknown bare path redirected to %q", rec.Header().Get("Location"))
	}
	// And a bare path never points at another user's instance.
	saveService(t, configDir, "bob", "secret", sink.endpoint(), nil)
	srv.syncRoutes()
	if rec := srv.getAs(t, "alice", "/secret/x", ""); rec.Code == http.StatusFound {
		t.Fatalf("cross-user bare path redirected to %q", rec.Header().Get("Location"))
	}
}

// The reaper must not reap a service someone is working in: idleTTL measures
// traffic, and only the proxy sees traffic.
func TestProxiedTrafficRecordsActivity(t *testing.T) {
	srv, configDir := proxyGateway(t)
	sink := newProxySink(t)
	inst := saveService(t, configDir, "alice", "web", sink.endpoint(), nil)
	srv.syncRoutes()

	if !srv.serviceActivity.Last(inst.ID).IsZero() {
		t.Fatal("nothing has been served yet")
	}
	if rec := srv.getAs(t, "alice", "/proxy/alice/web/", ""); rec.Code != 200 {
		t.Fatalf("setup: %d", rec.Code)
	}
	at := srv.serviceActivity.Last(inst.ID)
	if at.IsZero() {
		t.Fatal("a proxied request must count as activity")
	}
	if time.Since(at) > time.Minute {
		t.Fatalf("activity timestamp looks wrong: %v", at)
	}

	// A static card is not SRCOS's to reap, so traffic through one is not
	// recorded as instance activity (and it never enters the table).
	cardSink := newProxySink(t)
	writeCard(t, configDir, "/jupyter", cardSink.endpoint(), true)
	srv.registry.Reload()
	if rec := srv.getAs(t, "alice", "/proxy/alice/jupyter/", ""); rec.Code != 200 {
		t.Fatalf("card setup: %d", rec.Code)
	}
	if !srv.serviceActivity.Last("alice-web-svc").IsZero() && len(cardSink.paths) == 0 {
		t.Fatal("the card did not serve")
	}
	if _, ok := srv.routes.Get("alice", "jupyter"); ok {
		t.Fatalf("a card must not enter the dynamic table: %+v", srv.routes.List())
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 「启动中」进度页 / 失败说明
// ─────────────────────────────────────────────────────────────────────────

// A browser that lands on a starting instance gets a self-refreshing progress
// page with the log tail, not a 404 — and a program gets the status code.
func TestStartingInstanceShowsAProgressPage(t *testing.T) {
	srv, configDir := proxyGateway(t)
	inst := saveService(t, configDir, "alice", "web", "127.0.0.1:20010", func(i *runtime.Instance) {
		i.State = runtime.StateStarting
	})
	if err := os.MkdirAll(filepath.Dir(inst.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("loading reference index...\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.syncRoutes()

	rec := srv.getAsBrowser(t, "alice", "/proxy/alice/web/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for a starting service", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"服务启动中", "loading reference index", `http-equiv="refresh"`, "/proxy/alice/web/"} {
		if !strings.Contains(body, want) {
			t.Errorf("progress page is missing %q", want)
		}
	}

	// A program asking for data gets a status line, not a page.
	prog := srv.getAs(t, "alice", "/proxy/alice/web/", "")
	if prog.Code != http.StatusServiceUnavailable || strings.Contains(prog.Body.String(), "<html") {
		t.Fatalf("a non-HTML client must get a status line: %d %q", prog.Code, prog.Body)
	}
}

// A dead instance explains itself (state + error + log) instead of a bare 502.
func TestFailedInstanceShowsTheReason(t *testing.T) {
	srv, configDir := proxyGateway(t)
	inst := saveService(t, configDir, "alice", "web", "127.0.0.1:20011", func(i *runtime.Instance) {
		i.State = runtime.StateFailed
		i.Error = "healthcheck: not ready after 20s"
	})
	if err := os.MkdirAll(filepath.Dir(inst.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.LogPath, []byte("Traceback: no module named x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv.syncRoutes()

	rec := srv.getAsBrowser(t, "alice", "/proxy/alice/web/anything")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"服务未在运行", "healthcheck", "no module named x", "srcos svc start --tool web"} {
		if !strings.Contains(body, want) {
			t.Errorf("failure page is missing %q\n%s", want, body)
		}
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("a dead instance must not ask the browser to retry")
	}
}

// The status page only answers where nothing else can: an unknown tool is still
// a 404, and a running-but-unroutable instance is a bug, not a page.
func TestStatusPageDoesNotShadowOtherAnswers(t *testing.T) {
	srv, configDir := proxyGateway(t)

	// No record at all.
	if rec := srv.getAsBrowser(t, "alice", "/proxy/alice/nothing/"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown tool = %d", rec.Code)
	}

	// Running, but with no usable endpoint: the route table refused it, so the
	// honest answer is 404 rather than a progress page.
	saveService(t, configDir, "alice", "web", "10.0.0.5:80", nil)
	srv.syncRoutes()
	if rec := srv.getAsBrowser(t, "alice", "/proxy/alice/web/"); rec.Code != http.StatusNotFound {
		t.Fatalf("unroutable running instance = %d", rec.Code)
	}

	// A stopped instance does not shadow a working card on the same path.
	sink := newProxySink(t)
	writeCard(t, configDir, "/web", sink.endpoint(), true)
	srv.registry.Reload()
	if err := runtime.SaveInstance(runtime.InstancePath(configDir, runtime.InstanceID("alice", "web", "")),
		&runtime.Instance{
			ID: "alice-web-svc", User: "alice", Tool: "web", Kind: "service",
			State: runtime.StateStopped, Endpoint: "127.0.0.1:20012",
		}); err != nil {
		t.Fatal(err)
	}
	srv.syncRoutes()
	rec := srv.getAsBrowser(t, "alice", "/proxy/alice/web/x")
	if rec.Code != 200 || rec.Body.String() != "backend:/x" {
		t.Fatalf("the card must win over a stopped instance: %d %q", rec.Code, rec.Body)
	}
}

// Cross-user: the status page must not tell one user about another's instance.
func TestStatusPageIsScopedToTheSessionUser(t *testing.T) {
	srv, configDir := proxyGateway(t)
	saveService(t, configDir, "bob", "web", "127.0.0.1:20013", func(i *runtime.Instance) {
		i.State = runtime.StateStarting
	})
	srv.syncRoutes()

	if rec := srv.getAsBrowser(t, "alice", "/proxy/bob/web/"); rec.Code != http.StatusNotFound {
		t.Fatalf("another user's starting instance = %d", rec.Code)
	}
	if rec := srv.getAsBrowser(t, "bob", "/proxy/bob/web/"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("the owner must see the progress page: %d", rec.Code)
	}
}
