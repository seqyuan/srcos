package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
)

// seedResources puts a few files where the srcos:// scopes look for them.
func seedResources(t *testing.T, h *toolsHarness) {
	t.Helper()
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	home := config.HomeDir(h.configDir, "alice")
	write(filepath.Join(home, "notes.md"), "# Title\n\nbody\n")
	write(filepath.Join(home, "rows.csv"), "a,b\n1,2\n")
	write(filepath.Join(home, "plain.txt"), "hello\n")
	write(filepath.Join(home, "page.html"), "<html><body><script>1</script></body></html>")
	write(filepath.Join(home, "pic.png"), "\x89PNG\r\n\x1a\n")

	ws := config.WorkspaceDir(h.configDir, "alice", "demo")
	write(filepath.Join(ws, "out.txt"), "out\n")
}

func TestResourceScopesAndMetadata(t *testing.T) {
	h := newToolsHarness(t, nil)
	seedResources(t, h)

	// No src: the roots the user may browse.
	rec := h.get(t, "/api/resources")
	if rec.Code != 200 {
		t.Fatalf("scopes: %d %s", rec.Code, rec.Body)
	}
	var scopes struct {
		Scopes []struct {
			Scope string `json:"scope"`
			Kind  string `json:"kind"`
			Tool  string `json:"tool"`
		} `json:"scopes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scopes); err != nil {
		t.Fatal(err)
	}
	if len(scopes.Scopes) < 3 {
		t.Fatalf("scopes = %+v", scopes.Scopes)
	}

	// Metadata for one address.
	rec = h.get(t, "/api/resources?src="+encode("srcos://file/home/notes.md"))
	if rec.Code != 200 {
		t.Fatalf("metadata: %d %s", rec.Code, rec.Body)
	}
	var view struct {
		SandboxPath string `json:"sandboxPath"`
		IsDir       bool   `json:"isDir"`
		Mode        string `json:"mode"`
		Viewer      struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"viewer"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.SandboxPath != "/home/alice/notes.md" || view.Viewer.Kind != "markdown" {
		t.Fatalf("metadata = %+v", view)
	}
	if view.Text != "" {
		t.Error("the metadata endpoint must not carry the payload (that is /raw)")
	}

	// An explicit viewer wins.
	rec = h.get(t, "/api/resources?src="+encode("srcos://file/home/notes.md")+"&viewer=text")
	if !strings.Contains(rec.Body.String(), `"id":"text"`) {
		t.Errorf("viewer override ignored: %s", rec.Body)
	}

	// The workspace scope needs its tool, and the tool lives inside the address
	// (as ?tool=), not in the outer query.
	rec = h.get(t, "/api/resources?src="+encode("srcos://file/workspace/out.txt?tool=demo"))
	if rec.Code != 200 {
		t.Errorf("workspace scope: %d %s", rec.Code, rec.Body)
	}
	// Without it, the address is incomplete and refused.
	rec = h.get(t, "/api/resources?src="+encode("srcos://file/workspace/out.txt"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("workspace without a tool = %d, want 400", rec.Code)
	}
}

func TestResourceRawAndHTML(t *testing.T) {
	h := newToolsHarness(t, nil)
	seedResources(t, h)

	// Text is served as text/plain.
	rec := h.get(t, "/api/resources/raw?src="+encode("srcos://file/home/plain.txt"))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("raw txt: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != "hello\n" {
		t.Errorf("raw body = %q", rec.Body.String())
	}

	// An image keeps its type so the <img> works.
	rec = h.get(t, "/api/resources/raw?src="+encode("srcos://file/home/pic.png"))
	if rec.Header().Get("Content-Type") != "image/png" {
		t.Errorf("raw png content type = %q", rec.Header().Get("Content-Type"))
	}

	// HTML is served as text/plain by raw: never execute a user document here.
	rec = h.get(t, "/api/resources/raw?src="+encode("srcos://file/home/page.html"))
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("raw html content type = %q, want text/plain", got)
	}

	// The sandboxed door serves it as HTML with a CSP sandbox.
	rec = h.get(t, "/api/resources/html?src="+encode("srcos://file/home/page.html"))
	if rec.Code != 200 {
		t.Fatalf("html: %d %s", rec.Code, rec.Body)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("html content type = %q", rec.Header().Get("Content-Type"))
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "sandbox") || strings.Contains(csp, "allow-same-origin") {
		t.Errorf("CSP = %q, want a sandbox without allow-same-origin", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "" {
		t.Error("the html endpoint must be frameable (no X-Frame-Options)")
	}

	// The html endpoint refuses anything that is not HTML.
	rec = h.get(t, "/api/resources/html?src="+encode("srcos://file/home/plain.txt"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("html on a .txt = %d, want 400", rec.Code)
	}
}

func TestResourceErrors(t *testing.T) {
	h := newToolsHarness(t, nil)
	seedResources(t, h)

	noAuth := httptest.NewRequest("GET", "/api/resources?src="+encode("srcos://file/home/plain.txt"), nil)
	rec := httptest.NewRecorder()
	if !h.ServeHTTP(rec, noAuth) {
		t.Fatal("ServeHTTP declined")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}

	cases := []struct {
		url  string
		want int
	}{
		{"/api/resources?src=" + encode("nonsense"), http.StatusBadRequest},                          // bad grammar
		{"/api/resources?src=" + encode("srcos://file/nope/x"), http.StatusNotFound},                 // unknown storage
		{"/api/resources?src=" + encode("srcos://file/home/../../etc/passwd"), http.StatusForbidden}, // jail
		{"/api/resources?src=" + encode("srcos://file/home/plain.txt") + "&viewer=zzz", http.StatusBadRequest},
		{"/api/resources/raw?src=" + encode("srcos://file/home/missing.txt"), http.StatusNotFound},
	}
	for _, c := range cases {
		rec := h.get(t, c.url)
		if rec.Code != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.url, rec.Code, c.want, rec.Body)
		}
	}
}

// encode percent-encodes an address for use as a query value, the way a browser
// would when it builds the link.
func encode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			const hex = "0123456789ABCDEF"
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
