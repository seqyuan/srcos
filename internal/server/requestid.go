package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// requestIDHeader is the correlation header. An incoming value is a hint, not
// an identity or an audit field; it is accepted only if it is a safe token.
const requestIDHeader = "X-Request-ID"

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// requestIDHandler sets X-Request-ID before the handler runs and logs one line
// for gateway-owned requests. Proxy traffic, static assets and the probes are
// deliberately excluded: they are high-volume or scrape noise, and the proxy
// already has the backend's own logs.
func requestIDHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if !requestIDPattern.MatchString(id) {
			id = newRequestID()
		}
		// Before ServeHTTP: a streamed/hijacked response may never come back.
		w.Header().Set(requestIDHeader, id)

		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		if sw.hijacked || !logRequest(r.URL.Path) {
			return
		}
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		slog.Info("request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}

// logRequest logs gateway-owned paths. Invert the skip list (rather than
// allowlisting pages) so a new page is covered without remembering this file.
func logRequest(path string) bool {
	switch {
	case strings.HasPrefix(path, "/-/"), strings.HasPrefix(path, "/proxy/"),
		strings.HasPrefix(path, "/assets/"), path == "/favicon.ico":
		return false
	}
	return true
}

// statusWriter keeps the status without swallowing the interfaces streaming
// handlers assert directly: the API's SSE tail does w.(http.Flusher), and the
// proxy does w.(http.Hijacker). Unwrap keeps http.ResponseController working.
type statusWriter struct {
	http.ResponseWriter
	status   int
	wrote    bool
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	w.hijacked = true
	return h.Hijack()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
