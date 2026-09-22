package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/auth"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/web"
)

// adminGateway builds a gateway with one admin (root), one ordinary user
// (alice), a tool package and a running instance, so the console has something
// to show.
func adminGateway(t *testing.T) *Server {
	t.Helper()
	configDir := t.TempDir()
	for _, user := range []string{"root", "alice"} {
		writeUser(t, configDir, user)
	}
	policy, err := grant.New(map[string][]string{"bio": {"alice"}}, []string{"root"}, []grant.Grant{
		{Tool: "web", Groups: []string{"bio"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := grant.Save(config.GrantsPath(configDir), policy); err != nil {
		t.Fatal(err)
	}

	toolsDir := t.TempDir()
	writeServiceTool(t, toolsDir, "", "")

	pid, start, _ := runningChild(t)
	paths := runtime.PathsFor(configDir, "alice", "web", "")
	if err := os.MkdirAll(filepath.Dir(paths.RecordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LogPath, []byte("service booted\nlistening\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID: runtime.InstanceID("alice", "web", ""), User: "alice", Tool: "web", Kind: "service",
		JobName: "web", State: runtime.StateRunning, Backend: "local", Sandbox: "none",
		Endpoint: "127.0.0.1:20020", RoutePath: "/proxy/alice/web", Limiter: "none",
		PID: pid, PIDStart: start, LogPath: paths.LogPath, StartedAt: time.Now().UTC(),
	}
	if err := runtime.SaveInstance(paths.RecordPath, inst); err != nil {
		t.Fatal(err)
	}

	routes := route.NewTable()
	runner := runtime.NewRunner(runtime.Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		Routes:    routes,
		Backends:  map[string]runtime.Backend{"local": &runtime.Local{SystemdUser: boolPtr(false)}},
	})
	runner.SetPorts(portpool.New(0, 0))

	return NewWithOptions(&config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 30152},
		Auth:   config.AuthState{SessionSecret: "testsecret", SessionTTL: 86400},
	}, configDir, Options{ToolsDir: toolsDir, Supervisor: runner, Routes: routes})
}

func adminGet(t *testing.T, srv *Server, path, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, user, auth.SessionRev(strings.Repeat("1", 64)), false))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// The console is for admins and shows the facts an operator needs: every user's
// instances (with usage), the authorization state of every tool, and the groups.
func TestAdminPageRendersForAdmins(t *testing.T) {
	srv := adminGateway(t)

	rec := adminGet(t, srv, "/admin", "root")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"管理控制台",
		"alice-web-svc",      // another user's instance
		"127.0.0.1:20020",    // its endpoint
		"组:bio",              // the grant's subject
		`data-tool="web"`,    // the editor is wired to this tool
		"var ADMIN_STATE = ", // the action layer's state
		"/api/admin/grants/", // …which posts here
		"强制停止",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("console is missing %q", want)
		}
	}
}

// A non-admin is redirected to the dashboard: the console is not a secret, but
// it is not theirs either (and the API behind it refuses them regardless).
func TestAdminPageRedirectsNonAdmins(t *testing.T) {
	srv := adminGateway(t)
	rec := adminGet(t, srv, "/admin", "alice")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("non-admin: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Not logged in at all: the usual login redirect, with a return path.
	req := httptest.NewRequest("GET", "/admin", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("anonymous: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// The dashboard shows the console link only to admins.
func TestDashboardLinksTheConsoleForAdminsOnly(t *testing.T) {
	srv := adminGateway(t)
	if body := adminGet(t, srv, "/", "root").Body.String(); !strings.Contains(body, `href="/admin"`) {
		t.Error("an admin's dashboard should link the console")
	}
	if body := adminGet(t, srv, "/", "alice").Body.String(); strings.Contains(body, `href="/admin"`) {
		t.Error("a non-admin's dashboard should not link the console")
	}
}

// Stopping another user's instance is the operation the console exists for; it
// must go through the same StopService the reaper uses, so the record stays
// truthful.
func TestAdminForceStopStopsAndRecords(t *testing.T) {
	srv := adminGateway(t)
	inst := "alice-web-svc"

	req := httptest.NewRequest("POST", "/api/admin/instances/"+inst+"/stop", nil)
	req.Header.Set("Cookie", auth.SetSessionCookie("testsecret", 86400, "root", auth.SessionRev(strings.Repeat("1", 64)), false))
	req.Header.Set("Origin", "http://gw:30152")
	req.Host = "gw:30152"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("stop: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"state":"stopped"`) {
		t.Fatalf("the answer must report the new state: %s", rec.Body)
	}

	saved, err := runtime.LoadInstance(runtime.InstancePath(config.DirOf(srv.registry), inst))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.State.Terminal() {
		t.Fatalf("the record must agree: %s", saved.State)
	}
}

// Hand-editing grants.yaml used to need a restart, while the console is
// immediate. One of those had to give: the scan tick now notices the file.
func TestHandEditedPolicyIsPickedUpWithoutARestart(t *testing.T) {
	srv := adminGateway(t)
	policy, ok := srv.grants.(*grant.Policy)
	if !ok {
		t.Fatal("the harness should hold a real policy")
	}
	if !policy.Allowed("alice", "web") {
		t.Fatal("setup: alice is granted via the bio group")
	}

	// Rewrite the file by hand, the way an operator with vim would.
	updated := "admins:\n  - root\ngrants:\n  - tool: web\n    public: true\n"
	if err := os.WriteFile(config.GrantsPath(config.DirOf(srv.registry)), []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same second, so the modification time has to be forced forward: this is
	// exactly the case a naive mtime check would miss.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(config.GrantsPath(config.DirOf(srv.registry)), future, future); err != nil {
		t.Fatal(err)
	}

	srv.reloadPolicyIfChanged()

	if !policy.Allowed("nobody", "web") {
		t.Fatal("the hand edit must take effect on the live policy")
	}
	if len(policy.GroupsOf("alice")) != 0 {
		t.Fatal("the old groups must be gone")
	}

	// A broken file keeps the policy in memory rather than denying everything.
	if err := os.WriteFile(config.GrantsPath(config.DirOf(srv.registry)), []byte("grants: [not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future = future.Add(2 * time.Second)
	if err := os.Chtimes(config.GrantsPath(config.DirOf(srv.registry)), future, future); err != nil {
		t.Fatal(err)
	}
	srv.reloadPolicyIfChanged()
	if !policy.Allowed("nobody", "web") {
		t.Fatal("a broken file must not change the in-memory policy")
	}
}

// The canvas page is the one page served from the built frontend (ADR-012); the
// rest of the console stays a Go template. Both must exist and be admin-only.
func TestFlowEditorPageAndAssets(t *testing.T) {
	srv := adminGateway(t)

	// The page is served to an admin. With the frontend built, the flow id is
	// injected as bootstrap state (the bundle cannot know which flow it is
	// looking at); without a build, the page explains how to make one — both are
	// valid deployments, and `make build` must work without Node.
	rec := adminGet(t, srv, "/admin/flows/scrna/edit", "root")
	if rec.Code != 200 {
		t.Fatalf("editor page = %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if web.UIBuilt() {
		for _, want := range []string{"__SRCOS__", `"flowId":"scrna"`, `"user":"root"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the editor shell is missing %s", want)
			}
		}
	} else if !strings.Contains(body, "流程画布未构建") || !strings.Contains(body, "make webui") {
		t.Errorf("without a build the page must say how to make one:\n%s", body)
	}
	// A flow id that is not a slug is a 404, not a path into the filesystem.
	if rec := adminGet(t, srv, "/admin/flows/..%2f..%2fetc/edit", "root"); rec.Code != 404 {
		t.Fatalf("a path-shaped flow id = %d", rec.Code)
	}
	// Non-admins are redirected, like every other console page.
	if rec := adminGet(t, srv, "/admin/flows/scrna/edit", "alice"); rec.Code != http.StatusFound {
		t.Fatalf("non-admin editor page = %d", rec.Code)
	}

	// The assets endpoint serves whatever was built; with no build in the
	// repository it fails closed (404) rather than serving a directory listing.
	uiGet := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		out := httptest.NewRecorder()
		srv.Handler().ServeHTTP(out, req)
		return out
	}
	if web.UIBuilt() {
		shell := uiGet("/ui/")
		if shell.Code != 200 || !strings.Contains(shell.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("the shell = %d %q", shell.Code, shell.Header().Get("Content-Type"))
		}
		if got := uiGet("/ui/assets/nonexistent.js"); got.Body.String() != shell.Body.String() {
			t.Fatal("an unknown path under /ui/ must fall back to the app shell (it is a single-page app)")
		}
		// Built assets are content-hashed, so they may be cached hard; the shell
		// must not be, or a rebuilt bundle would not be picked up.
		if cc := shell.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Fatalf("shell cache-control = %q", cc)
		}
	} else if got := uiGet("/ui/"); got.Code != 404 {
		t.Fatalf("without a build the asset endpoint must 404, got %d", got.Code)
	}
}
