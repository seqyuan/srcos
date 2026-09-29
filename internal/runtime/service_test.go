package runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/tool"
)

// serviceHarness wires a runner with a port pool and a routing table, and a
// service tool whose work.sh serves HTTP on $SRCOS_PORT.
type serviceHarness struct {
	*harness
	routes *route.Table
	ports  *portpool.Pool
}

// httpServiceScript is deliberately dependency-free apart from python3, and it
// blocks — which is what makes it a service rather than a task.
const httpServiceScript = `#!/usr/bin/env bash
set -euo pipefail
: "${SRCOS_PORT:?SRCOS_PORT not set — the port pool must hand one over}"
echo "[svc] listening on ${SRCOS_PORT} in ${SRCOS_WORKSPACE}"
exec python3 -m http.server "${SRCOS_PORT}" --bind 127.0.0.1 --directory "${SRCOS_WORKSPACE}"
`

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 is required for the service fixture")
	}
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toolDir := filepath.Join(root, "tools", "web")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `
schemaVersion: 1
id: web
version: 0.1.0
name: Web Demo
kind: service
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: greeting, type: string, default: "hi"}
resources: {cpu: 1, memory: "512Mi"}
ingress:
  port: 8080
  healthcheck: {path: "/", timeout: "0:00:20"}
lifecycle:
  restart: never
  max_lifetime: "1h"
  idle_ttl: "30m"
`
	if err := os.WriteFile(filepath.Join(toolDir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "work.sh"), []byte(httpServiceScript), 0o755); err != nil {
		t.Fatal(err)
	}

	ports := portpool.New(24100, 24120)
	routes := route.NewTable()
	runner := NewRunner(Options{
		ConfigDir: configDir,
		ToolsDir:  filepath.Join(root, "tools"),
		User:      "alice",
		Backends:  map[string]Backend{"local": &Local{SystemdUser: boolPtr(false)}, "external": &External{}},
		Routes:    routes,
	})
	runner.SetPorts(ports)

	return &serviceHarness{
		harness: &harness{
			configDir: configDir,
			toolsDir:  filepath.Join(root, "tools"),
			toolDir:   toolDir,
			user:      "alice",
			runner:    runner,
		},
		routes: routes,
		ports:  ports,
	}
}

func TestStartServicePublishesRouteAndStops(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	ctx := context.Background()

	inst, err := h.runner.StartService(ctx, tl, nil)
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	// Every exit path from here must not leak the process.
	defer func() { _ = h.runner.StopService(ctx, inst) }()

	if inst.State != StateRunning {
		t.Fatalf("state = %s (%s)\nlog:\n%s", inst.State, inst.Error, readFile(t, inst.LogPath))
	}
	if inst.Endpoint == "" {
		t.Fatal("a service must record its endpoint")
	}
	if inst.Healthcheck != "ok" {
		t.Fatalf("healthcheck = %q", inst.Healthcheck)
	}

	// The service is reachable on the endpoint the pool handed out.
	resp, err := http.Get("http://" + inst.Endpoint + "/")
	if err != nil {
		t.Fatalf("the service is not reachable at %s: %v", inst.Endpoint, err)
	}
	resp.Body.Close()

	// And the proxy can find it by path.
	e, rest, ok := h.routes.GetByPath("/proxy/alice/web/")
	if !ok {
		t.Fatal("route was not published")
	}
	if e.Target.String() != inst.Endpoint {
		t.Fatalf("route target %s != endpoint %s", e.Target.String(), inst.Endpoint)
	}
	if rest != "" {
		t.Fatalf("rest = %q", rest)
	}

	// The port is held, so a second service cannot take it.
	if _, port, err := splitEndpoint(inst.Endpoint); err == nil {
		if owner, ok := h.ports.Owner(port); !ok || owner != inst.ID {
			t.Fatalf("port %d owner = %q", port, owner)
		}
	}

	// Stopping withdraws the route before killing the process, so a request
	// arriving mid-stop gets a "starting" page rather than a 502.
	if err := h.runner.StopService(ctx, inst); err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if _, _, ok := h.routes.GetByPath("/proxy/alice/web/"); ok {
		t.Fatal("route survived the stop")
	}

	saved, err := LoadInstance(InstancePath(h.configDir, inst.ID))
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != StateStopped {
		t.Fatalf("recorded state = %s", saved.State)
	}

	// The stop has to actually end the process. In the degraded mode (no user
	// systemd, ADR-014) there is no unit to stop by name, so the recorded pid is
	// the only handle there is — and a service that survives its own stop is
	// exactly how the port pool got exhausted and instances looked stopped while
	// still serving (found 2026-09-22).
	if inst.PID <= 0 || inst.PIDStart == 0 {
		t.Fatalf("a degraded service must record its pid and start time: pid=%d start=%d", inst.PID, inst.PIDStart)
	}
	if saved.PID != inst.PID || saved.PIDStart != inst.PIDStart {
		t.Fatalf("the record lost the pid: %d/%d vs %d/%d", saved.PID, saved.PIDStart, inst.PID, inst.PIDStart)
	}
	if processAlive(inst.PID) {
		t.Fatalf("the service process %d survived its stop", inst.PID)
	}

	// And its endpoint must be free, not merely unreserved.
	_, port, err := splitEndpoint(inst.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.ports.Owner(port); ok {
		t.Fatalf("port %d is still reserved", port)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d is still bound: %v", port, err)
	}
	ln.Close()
}

func TestStartServiceFailsWhenHealthcheckNeverPasses(t *testing.T) {
	h := newServiceHarness(t)
	// A script that exits immediately: the process is never ready.
	if err := os.WriteFile(filepath.Join(h.toolDir, "work.sh"),
		[]byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tl := h.tool(t)

	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatalf("StartService: %v", err)
	}
	if inst.State != StateFailed {
		t.Fatalf("state = %s", inst.State)
	}
	if !strings.Contains(inst.Error, "healthcheck") {
		t.Fatalf("unexpected error: %s", inst.Error)
	}
	// A failed start must not leave a route behind, and must release its port.
	if h.routes.Len() != 0 {
		t.Fatal("a failed service published a route")
	}
	if used := h.ports.InUse(); len(used) != 0 {
		t.Fatalf("a failed service leaked port(s) %v", used)
	}
}

func TestStartServiceMakesThePortVisibleToTheTool(t *testing.T) {
	h := newServiceHarness(t)
	// The tool must be told which port to bind; otherwise the pool and the
	// tool would have to agree by convention.
	script := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "${SRCOS_PORT}" > "${SRCOS_WORKSPACE}/port.txt"
# Keep the process alive long enough to be healthchecked, using the port we
# were given so the probe succeeds.
exec python3 -m http.server "${SRCOS_PORT}" --bind 127.0.0.1 --directory "${SRCOS_WORKSPACE}"
`
	if err := os.WriteFile(filepath.Join(h.toolDir, "work.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()

	_, port, err := splitEndpoint(inst.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(readFile(t, filepath.Join(h.configDir, "..", "data", "ws", "alice", "web", "port.txt")))
	if got != strconv.Itoa(port) {
		t.Fatalf("tool saw SRCOS_PORT=%q, pool allocated %d", got, port)
	}
}

// After a restart the record is all that is left, so reconcile has to be able
// to tell a live degraded service from a dead one — otherwise it either orphans
// a running service (losing its route) or adopts a corpse (a card that 502s).
func TestReconcileAdoptsALiveDegradedService(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()
	if inst.PID <= 0 || inst.PIDStart == 0 {
		t.Fatalf("a degraded service must record its pid: pid=%d start=%d", inst.PID, inst.PIDStart)
	}

	// Simulate a restart: the routing table is empty again while the record
	// still says running, and no process holds a handle any more.
	h.routes.DeleteInstance(inst.User, inst.Tool, inst.ID)

	adopted, orphaned, err := h.runner.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !containsString(adopted, inst.ID) {
		t.Fatalf("a live service must be adopted: adopted=%v orphaned=%v", adopted, orphaned)
	}
	if _, _, ok := h.routes.GetByPath(inst.RoutePath); !ok {
		t.Fatal("an adopted instance must have its route re-published")
	}
	if _, port, err := splitEndpoint(inst.Endpoint); err == nil {
		if owner, ok := h.ports.Owner(port); !ok || owner != inst.ID {
			t.Fatalf("port %d must be re-pinned to %s, owner=%q", port, inst.ID, owner)
		}
	}
}

func TestReconcileMarksDeadInstancesStopped(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Kill the process behind SRCOS's back: the record still says running.
	if err := stopRecordedProcess(context.Background(), inst.PID, inst.PIDStart); err != nil {
		t.Fatal(err)
	}
	h.routes.DeleteInstance(inst.User, inst.Tool, inst.ID)

	adopted, orphaned, _ := h.runner.Reconcile(context.Background())
	if containsString(adopted, inst.ID) {
		t.Fatal("a service whose process is gone must not be adopted")
	}
	if !containsString(orphaned, inst.ID) {
		t.Fatalf("a dead service must be reported as orphaned: %v", orphaned)
	}
	saved, err := LoadInstance(InstancePath(h.configDir, inst.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.State.Terminal() {
		t.Fatalf("an orphaned instance must be marked terminal, got %s", saved.State)
	}
	if _, port, err := splitEndpoint(inst.Endpoint); err == nil {
		if _, ok := h.ports.Owner(port); ok {
			t.Fatalf("port %d must be released", port)
		}
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestReaperStopsIdleService(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}

	// idle_ttl is 30m; pretend an hour has passed since the last request.
	inst.LastActiveAt = time.Now().Add(-time.Hour)
	if err := SaveInstance(InstancePath(h.configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}

	reaper := &Reaper{Runner: h.runner}
	stopped, err := reaper.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(stopped) != 1 || !strings.Contains(stopped[0], "idle") {
		t.Fatalf("stopped = %v", stopped)
	}
	if _, _, ok := h.routes.GetByPath("/proxy/alice/web/"); ok {
		t.Fatal("the reaper left the route behind")
	}
}

// TestReaperSkipsServiceWithOpenWebSocket pins the rule that a notebook the
// user left open is not idle just because it is quiet.
func TestReaperSkipsServiceWithOpenWebSocket(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()

	inst.LastActiveAt = time.Now().Add(-time.Hour)
	if err := SaveInstance(InstancePath(h.configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}

	reaper := &Reaper{
		Runner:   h.runner,
		ActiveWS: func(id string) bool { return id == inst.ID },
	}
	stopped, err := reaper.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(stopped) != 0 {
		t.Fatalf("a service with an open WebSocket was reaped: %v", stopped)
	}
}

// TestReaperDoesNotIdleReapSGETools documents ADR-015: queue wait makes a
// restart expensive, so idle reaping is opt-in on a cluster. It writes the
// record directly because the point under test is the reaper's decision, not
// how the instance came to exist.
func TestReaperDoesNotIdleReapSGETools(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)

	// An SGE-declaring tool needs its manifest on disk for the reaper to read
	// the lifecycle back, so rewrite tool.yaml rather than the loaded struct.
	manifest := readFile(t, filepath.Join(h.toolDir, "tool.yaml"))
	mustWrite(t, filepath.Join(h.toolDir, "tool.yaml"), strings.Replace(manifest, "backend: local", "backend: sge", 1))

	inst := &Instance{
		ID:           InstanceID(h.user, tl.ID, ""),
		User:         h.user,
		Tool:         tl.ID,
		Kind:         string(tool.KindService),
		State:        StateRunning,
		Backend:      string(tool.BackendSGE),
		Sandbox:      string(tl.Sandbox),
		Endpoint:     "127.0.0.1:24119",
		RoutePath:    route.DefaultPath(h.user, tl.ID),
		LogPath:      filepath.Join(h.configDir, "fake.log"),
		StartedAt:    time.Now().Add(-10 * time.Minute), // inside max_lifetime (1h)
		LastActiveAt: time.Now().Add(-10 * time.Hour),   // far past idle_ttl (30m)
	}
	if err := SaveInstance(InstancePath(h.configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}

	reaper := &Reaper{Runner: h.runner}
	stopped, _ := reaper.Sweep(context.Background(), time.Now())
	if len(stopped) != 0 {
		t.Fatalf("an SGE service was idle-reaped: %v", stopped)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// idleTTL measures traffic, not time since start. The record only knows the
// latter, so a caller that can see the traffic (the proxy) supplies the former
// — without it, a notebook someone has been working in all afternoon gets
// reaped mid-thought.
func TestReaperHonoursObservedActivity(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()

	// The record says the service started an hour ago; the proxy saw traffic a
	// minute ago.
	inst.LastActiveAt = time.Now().Add(-time.Hour)
	if err := SaveInstance(InstancePath(h.configDir, inst.ID), inst); err != nil {
		t.Fatal(err)
	}

	reaper := &Reaper{
		Runner:     h.runner,
		LastActive: func(id string) time.Time { return time.Now().Add(-time.Minute) },
	}
	stopped, err := reaper.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(stopped) != 0 {
		t.Fatalf("a service with recent traffic must not be reaped: %v", stopped)
	}

	// With no observation at all the record stands on its own, and the old
	// behaviour (reap it) is what remains.
	plain := &Reaper{Runner: h.runner}
	stopped, err = plain.Sweep(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(stopped) != 1 {
		t.Fatalf("stopped = %v", stopped)
	}
}

// The published route's backend path must come from ingress.backend_path, never
// from the healthcheck path: the probe path says where to knock, not where the
// app lives. (Before this, a tool with a non-root healthcheck had *every*
// proxied request sent under that path.)
func TestRouteBackendPathComesFromIngress(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	tl.Ingress.BackendPath = "/app"

	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()

	e, ok := h.routes.Get("alice", "web")
	if !ok {
		t.Fatal("no route was published")
	}
	if e.BackendPath != "/app" {
		t.Fatalf("route BackendPath = %q, want /app (it must not come from the healthcheck path)", e.BackendPath)
	}
}

// A service started from a declarative command: ${SRCOS_PORT} must be expanded
// to the port the pool handed over — the healthcheck only passes if the process
// actually listened on it.
func TestDeclarativeCommandRunsForAService(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	tl.Entry = ""
	tl.Command = []string{"bash", "-c",
		"python3 -m http.server ${SRCOS_PORT} --bind 127.0.0.1 --directory ${SRCOS_WORKSPACE}"}

	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()

	if inst.State != StateRunning {
		t.Fatalf("state = %s (%s) — ${SRCOS_PORT} probably did not reach the process", inst.State, inst.Error)
	}
	e, ok := h.routes.Get("alice", "web")
	if !ok || e.Target.Port == 0 {
		t.Fatalf("no route published: %+v", e)
	}
}

// A service's websocket / bandwidth declaration must reach the route the proxy
// reads — they are the same two fields a static card carries, so an instance and
// a card behave identically through the shared forwarding path.
func TestRouteCarriesWebSocketAndBandwidth(t *testing.T) {
	// Default: upgrades allowed, no throttle.
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()
	if e, ok := h.routes.Get("alice", "web"); !ok {
		t.Fatal("no route was published")
	} else if !e.WebSocket {
		t.Fatal("websocket must default to enabled on the route")
	}

	// Explicit: upgrades refused, 2 MiB/s.
	h2 := newServiceHarness(t)
	tl2 := h.tool(t)
	no := false
	tl2.Ingress.WebSocket = &no
	tl2.Ingress.BWLimit = 2 << 20
	inst2, err := h2.runner.StartService(context.Background(), tl2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h2.runner.StopService(context.Background(), inst2) }()
	e, ok := h2.routes.Get("alice", "web")
	if !ok {
		t.Fatal("no route was published")
	}
	if e.WebSocket {
		t.Fatal("websocket: false must reach the route")
	}
	if e.BWLimit != 2<<20 {
		t.Fatalf("route bwlimit = %d, want %d", e.BWLimit, 2<<20)
	}
}

// An external backend forwards to something that already runs: SRCOS starts
// nothing, so it also stops nothing — and the route must point at the declared
// endpoint, never at a port from the pool.
func TestExternalBackendForwardsToARunningServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "forwarded-ok")
	}))
	defer upstream.Close()
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	h := newServiceHarness(t)
	tl := h.tool(t)
	tl.Backend = tool.BackendExternal
	tl.External = &tool.ExternalSpec{Host: host, Port: port}
	tl.Entry = ""

	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != StateRunning {
		t.Fatalf("state = %s (%s)", inst.State, inst.Error)
	}
	if want := strings.TrimPrefix(upstream.URL, "http://"); inst.Endpoint != want {
		t.Fatalf("endpoint = %s, want the declared %s (not a pool port)", inst.Endpoint, want)
	}
	e, ok := h.routes.Get("alice", "web")
	if !ok {
		t.Fatal("no route was published")
	}
	if e.Target.Port != port {
		t.Fatalf("route target = %+v, want the declared port %d", e.Target, port)
	}
	if isReapable(inst) {
		t.Fatal("a forwarded backend must never be reaped: idleTTL is not ours to enforce")
	}

	// Stopping the instance withdraws the route and records it stopped — and
	// leaves the upstream alone, because SRCOS did not start it.
	if err := h.runner.StopService(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.routes.Get("alice", "web"); ok {
		t.Fatal("the route should be withdrawn after a stop")
	}
	resp, err := http.Get(upstream.URL)
	if err != nil {
		t.Fatalf("the upstream must survive the instance: %v", err)
	}
	resp.Body.Close()
}

// A backend that publishes its own endpoint is authoritative; the pool port it
// briefly reserved must be handed back (this is what SGE's ssh -L tunnel needs).
func TestPublishedEndpointReplacesThePoolPort(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()
	target, err := route.ParseTarget(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	h := newServiceHarness(t)
	tl := h.tool(t)
	fb := &publishingBackend{target: target}
	h.runner.opts.Backends["publishes"] = fb
	tl.Backend = tool.BackendSGE // any backend name the runner knows; the fake ignores it
	h.runner.opts.Backends["sge"] = fb

	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.runner.StopService(context.Background(), inst) }()
	if inst.Endpoint != target.String() {
		t.Fatalf("endpoint = %s, want the published %s", inst.Endpoint, target.String())
	}
	if e, ok := h.routes.Get("alice", "web"); !ok || e.Target.String() != target.String() {
		t.Fatalf("route target = %+v, want %s", e, target.String())
	}
}

// publishingBackend stands in for a backend that decides its own endpoint (SGE
// over a rendezvous file, external from its manifest).
type publishingBackend struct{ target route.Target }

func (b *publishingBackend) Name() string { return "publishes" }

func (b *publishingBackend) Start(ctx context.Context, req StartRequest) (Handle, error) {
	return &externalHandle{target: b.target}, nil
}
