package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
)

// This file is the supervision half of item 4: the gateway re-adopts whatever
// is still alive when it starts, and reclaims what has outlived its lifecycle —
// without a CLI in the loop. Both use the same runner the CLI does, wired
// without a user (it acts on records, not on someone's behalf).

func boolPtr(b bool) *bool { return &b }

// supervisorFor builds the runner the gateway uses: local backend, port pool,
// no user.
func supervisorFor(configDir, toolsDir string, routes *route.Table) *runtime.Runner {
	runner := runtime.NewRunner(runtime.Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		Routes:    routes,
		// The fallback (no user systemd) is what the tests can control: a
		// recorded pid is either alive or it is not.
		Backends: map[string]runtime.Backend{"local": &runtime.Local{SystemdUser: boolPtr(false)}},
	})
	runner.SetPorts(portpool.New(0, 0))
	return runner
}

// writeServiceTool writes a service tool manifest, with a lifecycle so the
// reaper has ceilings to enforce.
func writeServiceTool(t *testing.T, toolsDir, idleTTL, maxLifetime string) {
	writeServiceToolID(t, toolsDir, "web", idleTTL, maxLifetime)
}

// writeServiceToolID is writeServiceTool for one tool id.
func writeServiceToolID(t *testing.T, toolsDir, toolID, idleTTL, maxLifetime string) {
	t.Helper()
	dir := filepath.Join(toolsDir, toolID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A service always declares a lifecycle (the manifest validator requires
	// one); max_lifetime defaults to an hour, and idle reaping is opt-in.
	if maxLifetime == "" {
		maxLifetime = "1h"
	}
	lifecycle := "lifecycle: {restart: never, max_lifetime: \"" + maxLifetime + "\""
	if idleTTL != "" {
		lifecycle += ", idle_ttl: \"" + idleTTL + "\""
	}
	lifecycle += "}\n"
	manifest := "schemaVersion: 1\nid: " + toolID + "\nversion: 0.1.0\nname: " + toolID + "\nkind: service\n" +
		"backend: local\nsandbox: none\nentry: work.sh\n" +
		"resources: {cpu: 1, memory: \"512Mi\"}\n" +
		"ingress: {port: 8080, healthcheck: {path: \"/\", timeout: \"0:00:20\"}}\n" + lifecycle
	if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// runningChild starts a real child process and returns its pid and start time,
// the way a degraded-mode (no user systemd) service is recorded.
func runningChild(t *testing.T) (int, uint64, *exec.Cmd) {
	t.Helper()
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("child %d did not exit", cmd.Process.Pid)
		}
	})
	start, ok := procStartTime(cmd.Process.Pid)
	if !ok {
		t.Skip("/proc is needed to identify a process")
	}
	return cmd.Process.Pid, start, cmd
}

// procStartTime reads a process's start time from /proc (field 22), which the
// runtime uses to tell one pid incarnation from another.
func procStartTime(pid int) (uint64, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	i := strings.LastIndexByte(string(data), ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[i+1:]))
	if len(fields) < 20 {
		return 0, false
	}
	var v uint64
	for _, c := range fields[19] {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}

// gatewayWithSupervisor builds a gateway that supervises instances.
func gatewayWithSupervisor(t *testing.T, configDir, toolsDir string) (*Server, *route.Table) {
	t.Helper()
	routes := route.NewTable()
	srv := NewWithOptions(&config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 30152},
		Auth:   config.AuthState{SessionSecret: "testsecret", SessionTTL: 86400},
	}, configDir, Options{
		ToolsDir:   toolsDir,
		Supervisor: supervisorFor(configDir, toolsDir, routes),
		Routes:     routes,
	})
	return srv, routes
}

// A restart must not take a running service offline, and must not keep serving
// a dead one either: reconcile decides between the two by probing.
func TestStartupReconcileAdoptsAndOrphans(t *testing.T) {
	configDir := t.TempDir()
	writeUser(t, configDir, "alice")
	toolsDir := t.TempDir()
	writeServiceTool(t, toolsDir, "", "")
	writeServiceToolID(t, toolsDir, "gone", "", "")

	// One instance whose process is really there, one whose record lies.
	pid, start, _ := runningChild(t)
	live := saveService(t, configDir, "alice", "web", "127.0.0.1:20001", func(i *runtime.Instance) {
		i.PID, i.PIDStart = pid, start
	})
	dead := saveService(t, configDir, "alice", "gone", "127.0.0.1:20002", nil)

	_, routes := gatewayWithSupervisor(t, configDir, toolsDir)

	if _, ok := routes.Get("alice", "web"); !ok {
		t.Fatal("a live instance must be adopted (its route republished)")
	}
	saved, err := runtime.LoadInstance(runtime.InstancePath(configDir, live.ID))
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != runtime.StateRunning {
		t.Fatalf("an adopted instance must still be running, got %s", saved.State)
	}

	if _, ok := routes.Get("alice", "gone"); ok {
		t.Fatal("a dead instance must not be published")
	}
	saved, err = runtime.LoadInstance(runtime.InstancePath(configDir, dead.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.State.Terminal() {
		t.Fatalf("a dead instance must be marked terminal, got %s", saved.State)
	}
}

// Without a supervisor there is nothing to probe with, so the gateway degrades
// to trusting the records — the honest best a read-only deployment can do.
func TestWithoutASupervisorTheRecordsAreTrusted(t *testing.T) {
	configDir := t.TempDir()
	writeUser(t, configDir, "alice")
	saveService(t, configDir, "alice", "web", "127.0.0.1:20003", nil)

	srv := New(&config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 30152},
		Auth:   config.AuthState{SessionSecret: "testsecret", SessionTTL: 86400},
	}, configDir)
	if _, ok := srv.routes.Get("alice", "web"); !ok {
		t.Fatal("without a runner the record is all there is: it must still be published")
	}
}

// idleTTL is enforced without a CLI: the gateway reaps on its own tick, and the
// tool package's lifecycle is the ceiling.
func TestGatewayReapsAnIdleServiceByItself(t *testing.T) {
	configDir := t.TempDir()
	writeUser(t, configDir, "alice")
	toolsDir := t.TempDir()
	// A long lifetime ceiling, so the only thing that can reclaim this service
	// is the idle rule.
	writeServiceTool(t, toolsDir, "1s", "24h")

	pid, start, _ := runningChild(t)
	inst := saveService(t, configDir, "alice", "web", "127.0.0.1:20004", func(i *runtime.Instance) {
		i.PID, i.PIDStart = pid, start
		// Started an hour ago and never used since.
		i.StartedAt = time.Now().Add(-time.Hour)
		i.LastActiveAt = time.Now().Add(-time.Hour)
	})

	srv, routes := gatewayWithSupervisor(t, configDir, toolsDir)
	if _, ok := routes.Get("alice", "web"); !ok {
		t.Fatal("setup: the instance should be published")
	}

	stopped := srv.reapServices()
	if len(stopped) != 1 || !strings.Contains(stopped[0], "idle") {
		t.Fatalf("stopped = %v, want one idle reap", stopped)
	}

	saved, err := runtime.LoadInstance(runtime.InstancePath(configDir, inst.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.State.Terminal() {
		t.Fatalf("an idle service must be reclaimed, got %s", saved.State)
	}
	if _, ok := routes.Get("alice", "web"); ok {
		t.Fatal("the route must be withdrawn with the service")
	}
}

// An open WebSocket is not idle, whatever the clock says, and traffic observed
// by the proxy resets the clock (idleTTL measures use, not age).
func TestGatewayDoesNotReapAnInUseService(t *testing.T) {
	configDir := t.TempDir()
	writeUser(t, configDir, "alice")
	toolsDir := t.TempDir()
	writeServiceTool(t, toolsDir, "1s", "24h")

	pid, start, _ := runningChild(t)
	inst := saveService(t, configDir, "alice", "web", "127.0.0.1:20005", func(i *runtime.Instance) {
		i.PID, i.PIDStart = pid, start
		i.StartedAt = time.Now().Add(-time.Hour)
		i.LastActiveAt = time.Now().Add(-time.Hour)
	})

	srv, _ := gatewayWithSupervisor(t, configDir, toolsDir)

	// A tunneled connection is open on this instance.
	srv.activeConns.Add(inst.ID)
	if stopped := srv.reapServices(); len(stopped) != 0 {
		t.Fatalf("a service with an open connection must not be reaped: %v", stopped)
	}
	srv.activeConns.Release(inst.ID)

	// Recent traffic through the proxy also protects it, even without a socket.
	if err := srv.serviceActivity.Touch(inst.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if stopped := srv.reapServices(); len(stopped) != 0 {
		t.Fatalf("a service with recent traffic must not be reaped: %v", stopped)
	}
}

// writeUser creates a registered user with no services.
func writeUser(t *testing.T, configDir, name string) {
	t.Helper()
	if err := os.MkdirAll(config.UsersDir(configDir), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
	if err := os.WriteFile(config.UserConfigPath(configDir, name), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}
