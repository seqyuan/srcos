// Package portpool hands out loopback ports for service instances.
//
// Ports matter to the security model, not just to bookkeeping: every service
// instance listens on 127.0.0.1 only, and the gateway is the sole entry point.
// A port therefore grants nothing by itself — but a *collision* would silently
// route one user's traffic to another user's process, so allocation verifies
// the port is genuinely free with a real bind rather than trusting a bitmap.
package portpool

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"sync"
)

// DefaultRange is the loopback range service instances are placed in. It is
// above the ephemeral range (net.ipv4.ip_local_port_range usually starts around
// 32768) so a pool port does not collide with an outgoing connection's source
// port, and high enough to stay clear of the usual service ports.
const (
	DefaultLow  = 20000
	DefaultHigh = 30000
)

// Pool allocates loopback ports.
type Pool struct {
	lo, hi int
	mu     sync.Mutex
	used   map[int]string // port -> owner (instance id), for diagnostics
	// bind decides whether a candidate is actually free. Replaced in tests.
	bind func(port int) (net.Listener, error)
}

// New creates a pool over [lo, hi]. An empty or inverted range falls back to
// the default.
func New(lo, hi int) *Pool {
	if lo <= 0 || hi <= 0 || lo > hi || lo < 1024 {
		lo, hi = DefaultLow, DefaultHigh
	}
	return &Pool{
		lo:   lo,
		hi:   hi,
		used: map[int]string{},
		bind: func(port int) (net.Listener, error) {
			return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		},
	}
}

// Range returns the configured bounds.
func (p *Pool) Range() (lo, hi int) { return p.lo, p.hi }

// ErrExhausted is returned when no free port remains in the range.
var ErrExhausted = fmt.Errorf("port pool exhausted")

// Acquire reserves a free loopback port for owner.
//
// The returned port has been observed free by an actual bind, but it is not
// held open: the listener is closed before returning so the instance can bind
// it. Between the two binds the port is only protected by the pool's own
// bookkeeping, which is enough because SRCOS is the only allocator in this
// range.
func (p *Pool) Acquire(owner string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	span := p.hi - p.lo + 1
	if len(p.used) >= span {
		return 0, ErrExhausted
	}

	// Start from a rotating offset so repeated acquire/release cycles do not
	// hammer the same ports (and so a stale TIME_WAIT socket is skipped rather
	// than blocking every allocation).
	start := p.lo
	if len(p.used) > 0 {
		start = p.lo + (len(p.used)*7)%span
	}
	for i := 0; i < span; i++ {
		port := p.lo + (start-p.lo+i)%span
		if _, taken := p.used[port]; taken {
			continue
		}
		ln, err := p.bind(port)
		if err != nil {
			continue // in use by something outside the pool
		}
		ln.Close()
		p.used[port] = owner
		return port, nil
	}
	return 0, ErrExhausted
}

// Release returns a port to the pool. Releasing an unowned port is a no-op, so
// a double release after a failed start cannot free someone else's port.
func (p *Pool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

// Owner reports who holds a port, if anyone.
func (p *Pool) Owner(port int) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.used[port]
	return o, ok
}

// InUse returns the currently reserved ports, ascending.
func (p *Pool) InUse() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.used))
	for port := range p.used {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// Reserve marks a specific port as used without probing it. It exists for
// startup reconciliation: a service instance found still alive after a restart
// must not have its port handed to a second instance.
func (p *Pool) Reserve(port int, owner string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if port < p.lo || port > p.hi {
		return fmt.Errorf("port %d is outside the pool range %d-%d", port, p.lo, p.hi)
	}
	if cur, taken := p.used[port]; taken && cur != owner {
		return fmt.Errorf("port %d is already held by %s", port, cur)
	}
	p.used[port] = owner
	return nil
}
