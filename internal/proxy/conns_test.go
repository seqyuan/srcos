package proxy

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hijackableWriter is a minimal ResponseWriter that can be hijacked, standing in
// for the real one net/http hands to a handler.
type hijackableWriter struct {
	http.ResponseWriter
	conn net.Conn
}

func (h *hijackableWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// A WebSocket counts as activity from the moment it is established until the
// connection closes — the reaper's question is "is anyone using this", and an
// open tunnel is the strongest yes.
func TestTrackHijackCountsUntilClose(t *testing.T) {
	conns := NewActiveConns()
	if conns.Active("alice-web-svc") {
		t.Fatal("nothing is connected yet")
	}

	server, client := net.Pipe()
	defer client.Close()
	w := TrackHijack(&hijackableWriter{ResponseWriter: httptest.NewRecorder(), conn: server}, "alice-web-svc", conns)

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	if !conns.Active("alice-web-svc") {
		t.Fatal("a hijacked connection must count as active")
	}
	if conns.Active("someone-else") {
		t.Fatal("the count must be per instance")
	}

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conns.Active("alice-web-svc") {
		t.Fatal("a closed connection must stop counting")
	}

	// Closing twice (the proxy may close it, then the tunnel logic too) must not
	// drive the count negative and hide a later session.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if conns.Active("alice-web-svc") {
		t.Fatal("still counting after a double close")
	}
}

// Wrapping must be a no-op for a static card (no instance id) and for a nil
// tracker, so the forwarding path can wrap unconditionally.
func TestTrackHijackOnlyWrapsInstanceRoutes(t *testing.T) {
	rec := httptest.NewRecorder()
	if got := TrackHijack(rec, "", NewActiveConns()); got != http.ResponseWriter(rec) {
		t.Fatal("a route without an instance must not be wrapped")
	}
	if got := TrackHijack(rec, "alice-web-svc", nil); got != http.ResponseWriter(rec) {
		t.Fatal("a nil tracker must not wrap")
	}
}

// The wrapper must stay transparent: net/http looks *through* ResponseWriters
// for Flush and friends (http.ResponseController), so Unwrap has to be there.
func TestTrackHijackKeepsTheWriterTransparent(t *testing.T) {
	recorder := httptest.NewRecorder()
	w := TrackHijack(recorder, "alice-web-svc", NewActiveConns())
	if _, ok := w.(interface{ Unwrap() http.ResponseWriter }); !ok {
		t.Fatal("the wrapper must unwrap")
	}
	w.WriteHeader(204)
	if recorder.Code != 204 {
		t.Fatalf("the wrapper swallowed the status: %d", recorder.Code)
	}
	// Flush is discoverable through the unwrapping mechanism.
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through the wrapper: %v", err)
	}
}
