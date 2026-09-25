package api

import (
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/resource"
)

// This file is the HTTP spelling of the srcos:// protocol (ADR-011/016). The
// answers come from package inspect, so the viewer page and this API resolve an
// address identically.
//
// Three endpoints, with a deliberate split:
//
//	GET /api/resources?src=...        metadata (+ a scope list when src is empty)
//	GET /api/resources/raw?src=...    the bytes, as a type that is safe to open
//	GET /api/resources/html?src=...   user HTML, for the sandboxed iframe only
//
// The split exists for one reason: **`raw` must never answer `text/html`**. A
// file viewer that returned user HTML under the gateway's origin would hand
// every same-origin script the session cookie and the management API
// (AGENTS.md 安全不变式). HTML therefore has its own endpoint, which carries a
// CSP `sandbox` so the document cannot reach its own origin even if someone
// navigates to it directly.

// resourceAddress parses the `src` query parameter.
func resourceAddress(r *http.Request) (resource.Address, inspect.ResourceRequest, error) {
	src := strings.TrimSpace(r.URL.Query().Get("src"))
	if src == "" {
		return resource.Address{}, inspect.ResourceRequest{}, fmt.Errorf("%w: src is required", resource.ErrInvalid)
	}
	addr, err := resource.Parse(src)
	if err != nil {
		return resource.Address{}, inspect.ResourceRequest{}, err
	}
	return addr, inspect.ResourceRequest{Scope: addr.Scope, Path: addr.Path, Tool: addr.Tool}, nil
}

// handleResource returns one address's metadata, or the list of browsable
// scopes when no address is given.
//
// It is the answer a file browser, an agent or a script needs before
// it fetches anything: the sandbox path, the size, the access mode, and which
// viewer claims it.
func (h *Handler) handleResource(w http.ResponseWriter, r *http.Request, username string) {
	if strings.TrimSpace(r.URL.Query().Get("src")) == "" {
		scopes, err := h.reader().ResourceScopes(username)
		if err != nil {
			writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"scopes": scopes})
		return
	}

	_, req, err := resourceAddress(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	view, err := h.reader().DescribeResource(username, req, r.URL.Query().Get("viewer"))
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	// The payload has its own endpoint; a metadata answer that also carried a
	// quarter megabyte of text would make the JSON useless for its purpose.
	view.Text = ""
	writeJSON(w, 200, view)
}

// handleResourceRaw streams a resource's bytes.
//
// The content type is chosen from a small safe set: inline-capable binaries
// (images, PDF) get their real type, everything else is `text/plain`. An
// address naming a `.html` file is served as text here on purpose — the
// sandboxed `/api/resources/html` endpoint is the only way to render it.
func (h *Handler) handleResourceRaw(w http.ResponseWriter, r *http.Request, username string) {
	addr, req, err := resourceAddress(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f, err := h.reader().OpenResource(username, req)
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	name := path.Base(addr.Path)
	if name == "." || name == "/" || name == "" {
		name = "resource"
	}
	w.Header().Set("Content-Type", rawContentType(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// handleResourceHTML serves a user HTML file for the sandboxed iframe.
//
// Two locks, because this is the one endpoint that hands the browser a document
// it will execute:
//
//   - `Content-Security-Policy: sandbox` without `allow-same-origin`, so the
//     document runs in an opaque origin: `document.cookie` is empty and a
//     same-origin fetch to the gateway carries no credential.
//   - the viewer page puts it in an `<iframe sandbox>` with the same tokens
//     (ADR-011's requirement). The CSP keeps the property even if someone opens
//     the URL directly, outside the iframe.
//
// Scripts stay allowed (an HTML report that cannot draw a chart is not a
// preview), which is exactly why the origin must not be the gateway's.
func (h *Handler) handleResourceHTML(w http.ResponseWriter, r *http.Request, username string) {
	addr, req, err := resourceAddress(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	name := path.Base(addr.Path)
	if !isHTMLName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("%s is not an HTML file; use /api/resources/raw", name),
		})
		return
	}
	f, err := h.reader().OpenResource(username, req)
	if err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	// Deliberately no X-Frame-Options: the viewer frames this on purpose, and
	// the sandbox above is what makes framing it safe.
	http.ServeContent(w, r, name, fi.ModTime(), f)
}

// rawContentType is the allowlist `raw` answers with.
//
// Anything not listed — including .html, .svg and .js — becomes text/plain, so
// a browser that follows a link to the raw endpoint displays text instead of
// executing it.
func rawContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".bmp":
		return "image/bmp"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	default:
		return "text/plain; charset=utf-8"
	}
}

func isHTMLName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return true
	}
	return false
}
