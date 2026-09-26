package runtime

import (
	"context"
	"net"
	"time"

	"github.com/seqyuan/srcos/internal/route"
)

// External forwards to a backend that already runs (design §3).
//
// It is the "port forwarding" case — what a static service card has always
// done — expressed as a backend so it shares one authorization policy (Grant),
// one audit stream and one catalogue with the services SRCOS starts itself.
//
// The difference from Local/SGE is ownership, and it is load-bearing:
// SRCOS did not start this process, so it never stops it, never reaps it and
// never claims its exit code. `svc stop` on an external instance withdraws the
// route and marks the record stopped; the process keeps running.
type External struct{}

func (b *External) Name() string { return "external" }

// Start publishes the declared endpoint. Nothing is launched.
func (b *External) Start(ctx context.Context, req StartRequest) (Handle, error) {
	spec := req.Tool.External
	if spec == nil {
		return nil, &ErrUnsupportedBackend{Tool: req.Tool.ID, Backend: string(req.Tool.Backend), Known: []string{"external"}}
	}
	target, err := route.ParseTarget(spec.Endpoint())
	if err != nil {
		return nil, err
	}
	return &externalHandle{target: target}, nil
}

// UnitAlive answers "is the forwarded backend still there" for reconciliation.
//
// It probes, because there is no process to ask and no record of an exit: a
// backend that stopped is how a forwarded instance ends.
func (b *External) UnitAlive(ctx context.Context, inst *Instance) bool {
	target, err := route.ParseTarget(inst.Endpoint)
	if err != nil {
		return false
	}
	return dialable(target)
}

type externalHandle struct {
	target route.Target
}

func (h *externalHandle) Ref() string { return "external" }

// Wait blocks until the context ends: there is no process whose exit we could
// observe, and an external backend has no exit code to report.
func (h *externalHandle) Wait(ctx context.Context) ExitStatus {
	<-ctx.Done()
	return ExitStatus{Code: -1, Err: ctx.Err()}
}

// Stop does nothing on purpose. SRCOS is not this process's parent; stopping it
// would be stopping something the platform does not own. What `svc stop` does is
// withdraw the route and record the instance as stopped.
func (h *externalHandle) Stop(context.Context) error { return nil }

func (h *externalHandle) Alive() bool { return dialable(h.target) }

func (h *externalHandle) Endpoint() (route.Target, bool) { return h.target, true }

// WantEndpoint is true: the manifest is authoritative about where the backend
// is, not the caller's port pool.
func (h *externalHandle) WantEndpoint() bool { return true }

func (h *externalHandle) Command() []string { return nil }

// Limiter is "none": no resource control applies to a process SRCOS does not
// start (should the backend's operator want limits, they belong to whoever
// started it).
func (h *externalHandle) Limiter() string { return "none" }

// dialable reports whether something accepts a TCP connection at the target.
func dialable(target route.Target) bool {
	conn, err := net.DialTimeout("tcp", target.String(), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
