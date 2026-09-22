package runtime

import (
	"context"
	"net/http"
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
		Backends:  map[string]Backend{"local": &Local{SystemdUser: boolPtr(false)}},
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
	defer func() { _ = h.runner.StopService(ctx, tl, inst) }()

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
	if err := h.runner.StopService(ctx, tl, inst); err != nil {
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
	defer func() { _ = h.runner.StopService(context.Background(), tl, inst) }()

	_, port, err := splitEndpoint(inst.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(readFile(t, filepath.Join(h.configDir, "..", "data", "ws", "alice", "web", "port.txt")))
	if got != strconv.Itoa(port) {
		t.Fatalf("tool saw SRCOS_PORT=%q, pool allocated %d", got, port)
	}
}

func TestReconcileMarksDeadInstancesStopped(t *testing.T) {
	h := newServiceHarness(t)
	tl := h.tool(t)
	inst, err := h.runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a SRCOS restart: the in-memory routing table is empty again
	// while the record still says running.
	h.routes.DeleteInstance(inst.User, inst.Tool, inst.ID)

	// Reconcile must not claim an instance is alive when its process is gone.
	adopted, orphaned, _ := h.runner.Reconcile(context.Background())
	// Either it was adopted (process still alive) or reported as orphaned; what
	// must never happen is a route pointing at nothing.
	for _, id := range adopted {
		if id == inst.ID {
			if _, _, ok := h.routes.GetByPath(inst.RoutePath); !ok {
				t.Fatal("an adopted instance must have its route re-published")
			}
		}
	}
	for _, id := range orphaned {
		if id == inst.ID {
			saved, err := LoadInstance(InstancePath(h.configDir, inst.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !saved.State.Terminal() {
				t.Fatalf("an orphaned instance must be marked terminal, got %s", saved.State)
			}
		}
	}
	_ = h.runner.StopService(context.Background(), tl, inst)
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
	defer func() { _ = h.runner.StopService(context.Background(), tl, inst) }()

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
