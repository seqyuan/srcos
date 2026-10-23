package server

import (
	"testing"

	"github.com/seqyuan/srcos/internal/runtime"
)

// The scan tick rebuilds every route from the instance record. Ingress settings
// that live only in tool.yaml (bwlimit, backend_path, an explicit websocket:
// false) must therefore be persisted on the record and survive the rebuild.
func TestRouteEntryKeepsPersistedIngressSettings(t *testing.T) {
	ws := false
	inst := &runtime.Instance{
		ID:       "alice-web-svc",
		User:     "alice",
		Tool:     "web",
		Kind:     "service",
		State:    runtime.StateRunning,
		Endpoint: "127.0.0.1:20001",
		Ingress: runtime.IngressRoute{
			WebSocket:   &ws,
			BWLimit:     1234,
			BackendPath: "/app",
			Configured:  true,
		},
	}
	e, ok := routeEntryFrom(inst, nil)
	if !ok {
		t.Fatal("a running service must be routable")
	}
	if e.WebSocket {
		t.Error("websocket: false was lost")
	}
	if e.BWLimit != 1234 {
		t.Errorf("BWLimit = %d, want 1234", e.BWLimit)
	}
	if e.BackendPath != "/app" {
		t.Errorf("BackendPath = %q, want /app", e.BackendPath)
	}
}

// A record written before ingress settings were persisted gets them from the
// tool package on the rebuild; this is the upgrade path for running services.
func TestRouteEntryFallsBackToToolForLegacyRecord(t *testing.T) {
	ws := false
	ing := runtime.IngressRoute{WebSocket: &ws, BWLimit: 99, BackendPath: "/legacy", Configured: true}
	inst := &runtime.Instance{
		ID:       "alice-web-svc",
		User:     "alice",
		Tool:     "web",
		Kind:     "service",
		State:    runtime.StateRunning,
		Endpoint: "127.0.0.1:20001",
	}
	e, ok := routeEntryFrom(inst, &ing)
	if !ok {
		t.Fatal("a running service must be routable")
	}
	if e.WebSocket || e.BWLimit != 99 || e.BackendPath != "/legacy" {
		t.Fatalf("legacy fallback not applied: %+v", e)
	}
}
