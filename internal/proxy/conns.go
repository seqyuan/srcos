package proxy

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"sync"
)

// ActiveConns counts long-lived (hijacked) connections per service instance.
//
// A WebSocket is the clearest evidence that a service is *in use*: a notebook
// or a terminal someone left open is not idle, however quiet the socket is
// (ADR-015). The reaper asks this question before reclaiming a service, so the
// count has to err on the safe side — an over-count only delays a reap, while an
// under-count would tear down a live session.
type ActiveConns struct {
	mu sync.Mutex
	n  map[string]int
}

// NewActiveConns builds an empty tracker.
func NewActiveConns() *ActiveConns { return &ActiveConns{n: map[string]int{}} }

// Active reports whether an instance currently has open hijacked connections.
func (a *ActiveConns) Active(instanceID string) bool {
	if a == nil || instanceID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n[instanceID] > 0
}

// Add records that an instance has one more open connection.
//
// It is exported because the count has to be fed from wherever connections are
// actually established — TrackHijack covers the proxy's own path, and a host
// that fronts SRCOS differently (or a test) can feed it directly. Forgetting a
// Release only delays a reap; the reverse mistake would kill a live session.
func (a *ActiveConns) Add(instanceID string) {
	if a == nil || instanceID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n[instanceID]++
}

// Release records that one connection of an instance closed.
func (a *ActiveConns) Release(instanceID string) {
	if a == nil || instanceID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n[instanceID] <= 1 {
		delete(a.n, instanceID)
		return
	}
	a.n[instanceID]--
}

// TrackHijack wraps a ResponseWriter so that a protocol switch (a WebSocket
// upgrade) is counted against an instance until the connection closes.
//
// It is a no-op when there is no instance to attribute (a static card) or no
// tracker. Unwrap is implemented so the wrapper stays transparent to the
// standard-library machinery that looks through ResponseWriters
// (http.ResponseController: Flush, SetWriteDeadline, …).
func TrackHijack(w http.ResponseWriter, instanceID string, conns *ActiveConns) http.ResponseWriter {
	if conns == nil || instanceID == "" {
		return w
	}
	return &hijackTracker{ResponseWriter: w, conns: conns, instanceID: instanceID}
}

type hijackTracker struct {
	http.ResponseWriter
	conns      *ActiveConns
	instanceID string
}

// Unwrap keeps every capability of the wrapped writer discoverable.
func (h *hijackTracker) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// Hijack performs the protocol switch and starts counting the connection.
//
// The count ends when the connection is closed; net/http's ReverseProxy closes
// the hijacked client connection itself when the tunnel finishes (it defers
// conn.Close in handleUpgradeResponse), so wrapping the returned net.Conn is
// enough to see the end of the session.
func (h *hijackTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := h.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("connection cannot be hijacked: %T does not implement http.Hijacker", h.ResponseWriter)
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	h.conns.Add(h.instanceID)
	return &trackedConn{Conn: conn, done: func() { h.conns.Release(h.instanceID) }}, rw, nil
}

// trackedConn releases the count exactly once, whatever the proxy does with the
// connection afterwards.
type trackedConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *trackedConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}
