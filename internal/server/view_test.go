package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
)

// viewGateway is proxyGateway plus a home directory with something in it, so
// the srcos:// viewer has a file to show.
func viewGateway(t *testing.T) *Server {
	t.Helper()
	srv, configDir := proxyGateway(t)
	home := config.HomeDir(configDir, "alice")
	if err := os.MkdirAll(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"notes.md":  "# Title\n\nbody **bold**\n",
		"rows.csv":  "a,b\n1,2\n",
		"plain.txt": "hello\n",
		"page.html": "<html><body>hi</body></html>",
		"sub/x.txt": "deep\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return srv
}

func viewURL(src string) string {
	return "/view?src=" + url.QueryEscape(src)
}

func TestViewPageRequiresLogin(t *testing.T) {
	srv := viewGateway(t)
	req := httptest.NewRequest("GET", viewURL("srcos://file/home/plain.txt"), nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/login") {
		t.Fatalf("unauthenticated /view = %d %q, want a redirect to /login",
			rec.Code, rec.Header().Get("Location"))
	}
}

func TestViewLandingListsScopes(t *testing.T) {
	srv := viewGateway(t)
	rec := srv.getAsBrowser(t, "alice", "/view")
	if rec.Code != 200 {
		t.Fatalf("landing: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "我的 home") {
		t.Errorf("landing page is missing the home scope:\n%s", body)
	}
	if !strings.Contains(body, "srcos://file/home") {
		t.Errorf("landing page is missing the address form:\n%s", body)
	}
}

func TestViewRendersPerViewer(t *testing.T) {
	srv := viewGateway(t)

	// Markdown: server-rendered, so the heading is in the HTML.
	rec := srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/notes.md"))
	if rec.Code != 200 {
		t.Fatalf("markdown: %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<h1>Title</h1>") || !strings.Contains(body, "<strong>bold</strong>") {
		t.Errorf("markdown was not rendered:\n%s", body)
	}
	if !strings.Contains(body, "/api/resources/raw?src=") {
		t.Errorf("the raw link is missing:\n%s", body)
	}

	// CSV: a real table.
	rec = srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/rows.csv"))
	if !strings.Contains(rec.Body.String(), "<table class=\"rv-table\"") {
		t.Errorf("csv was not rendered as a table:\n%s", rec.Body)
	}

	// Text: line numbers.
	rec = srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/plain.txt"))
	if !strings.Contains(rec.Body.String(), "rv-ln") {
		t.Errorf("text viewer is missing line numbers:\n%s", rec.Body)
	}

	// HTML: never inline, always a sandboxed iframe pointing at the CSP-protected
	// endpoint (ADR-011).
	rec = srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/page.html"))
	body = rec.Body.String()
	if !strings.Contains(body, `sandbox="allow-scripts`) {
		t.Errorf("html preview is not sandboxed:\n%s", body)
	}
	if !strings.Contains(body, "/api/resources/html?src=") {
		t.Errorf("html preview does not use the sandboxed endpoint:\n%s", body)
	}
	if strings.Contains(body, "<html><body>hi</body></html>") {
		t.Errorf("raw HTML was inlined into the gateway's own page:\n%s", body)
	}

	// A directory listing links to children.
	rec = srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/sub"))
	if !strings.Contains(rec.Body.String(), "x.txt") {
		t.Errorf("directory listing is missing its child:\n%s", rec.Body)
	}
}

func TestViewDirChildLinksAreScopeRelative(t *testing.T) {
	srv := viewGateway(t)
	// A directory listing at a nested path must link to the child's path inside
	// the scope, not join the child onto the directory again (which produced
	// `home/sub/sub/x.txt` and a 404 — caught by the browser e2e).
	rec := srv.getAsBrowser(t, "alice", viewURL("srcos://file/home/sub"))
	body := rec.Body.String()
	want := url.QueryEscape("srcos://file/home/sub/x.txt")
	if !strings.Contains(body, want) {
		t.Errorf("the child link is missing %s:\n%s", want, body)
	}
	if doubled := url.QueryEscape("srcos://file/home/sub/sub/x.txt"); strings.Contains(body, doubled) {
		t.Errorf("the child link doubled the directory:\n%s", body)
	}
}

func TestViewErrorsAndMethods(t *testing.T) {
	srv := viewGateway(t)

	cases := []struct {
		path string
		want int
	}{
		{viewURL("not-an-address"), http.StatusBadRequest},
		{viewURL("srcos://file/home/../../etc/passwd"), http.StatusForbidden},
		{viewURL("srcos://file/home/missing.txt"), http.StatusNotFound},
		{viewURL("srcos://file/nope/x"), http.StatusNotFound},
	}
	for _, c := range cases {
		rec := srv.getAsBrowser(t, "alice", c.path)
		if rec.Code != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.path, rec.Code, c.want, rec.Body)
		}
	}

	// Only GET renders a page.
	req := httptest.NewRequest("POST", "/view", nil)
	req.Header.Set("Cookie", sessionCookie("alice"))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST /view = %d, want 404", rec.Code)
	}
}
