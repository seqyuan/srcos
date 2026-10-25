package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the operations surface: liveness, readiness and a small
// Prometheus text endpoint. They are unauthenticated on purpose — a process
// supervisor or scrape cannot hold a session — and they expose facts about the
// deployment, never user data.

// readyCheck is one readiness dimension. A failed check makes the endpoint 503;
// the body says which one and why (without paths that are not already obvious
// from config).
type readyCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	checks := s.readyChecks()
	ready := true
	for _, c := range checks {
		if !c.OK {
			ready = false
			break
		}
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "checks": checks})
}

func (s *Server) readyChecks() []readyCheck {
	checks := []readyCheck{
		{Name: "config", OK: true, Detail: config.DirOf(s.registry)},
	}

	// Tools: a missing directory is a valid fresh deployment; a malformed
	// package or an unreadable directory is not.
	if strings.TrimSpace(s.toolsDir) == "" {
		checks = append(checks, readyCheck{Name: "tools", OK: true, Detail: "not configured"})
	} else if _, err := os.Stat(s.toolsDir); err != nil {
		if os.IsNotExist(err) {
			checks = append(checks, readyCheck{Name: "tools", OK: true, Detail: "none"})
		} else {
			checks = append(checks, readyCheck{Name: "tools", OK: false, Detail: err.Error()})
		}
	} else if tools, err := tool.Discover(s.toolsDir); err != nil {
		checks = append(checks, readyCheck{Name: "tools", OK: false, Detail: err.Error()})
	} else {
		checks = append(checks, readyCheck{Name: "tools", OK: true, Detail: fmt.Sprintf("%d tool(s)", len(tools))})
		// Only require bwrap when a registered tool actually asks for it: a
		// deployment whose tools are all sandbox: none is ready without it.
		needBwrap := false
		for _, t := range tools {
			if t.Sandbox == tool.SandboxBwrap {
				needBwrap = true
				break
			}
		}
		if needBwrap {
			path, ok, why := sandbox.BwrapProbe()
			detail := path
			if !ok {
				detail = why
			}
			checks = append(checks, readyCheck{Name: "bwrap", OK: ok, Detail: detail})
		}
	}

	if checker, ok := s.storages.(interface{ CheckReachable() []string }); ok {
		if problems := checker.CheckReachable(); len(problems) > 0 {
			checks = append(checks, readyCheck{Name: "storages", OK: false, Detail: strings.Join(problems, "; ")})
		} else {
			checks = append(checks, readyCheck{Name: "storages", OK: true})
		}
	}
	if checker, ok := s.environments.(interface{ CheckReachable() []string }); ok {
		if problems := checker.CheckReachable(); len(problems) > 0 {
			checks = append(checks, readyCheck{Name: "environments", OK: false, Detail: strings.Join(problems, "; ")})
		} else {
			checks = append(checks, readyCheck{Name: "environments", OK: true})
		}
	}
	checks = append(checks,
		readyCheck{Name: "runner", OK: true, Detail: runnerDetail(s.runner != nil)},
		readyCheck{Name: "task-queue", OK: true, Detail: runnerDetail(s.taskQueue != nil)},
	)
	return checks
}

func runnerDetail(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP srcos_up SRCOS process is running.\\n# TYPE srcos_up gauge\\nsrcos_up 1\\n")
	fmt.Fprintf(&b, "# HELP srcos_build_info Build information.\\n# TYPE srcos_build_info gauge\\n")
	fmt.Fprintf(&b, "srcos_build_info{version=%q} 1\\n", s.mcpVersion)

	counts := map[string]int{}
	if insts, err := runtime.ListInstances(config.DirOf(s.registry)); err == nil {
		for _, inst := range insts {
			counts[string(inst.State)]++
		}
	}
	states := make([]string, 0, len(counts))
	for state := range counts {
		states = append(states, state)
	}
	sort.Strings(states)
	fmt.Fprintf(&b, "# HELP srcos_instances Instance records by state.\\n# TYPE srcos_instances gauge\\n")
	for _, state := range states {
		fmt.Fprintf(&b, "srcos_instances{state=%q} %d\\n", state, counts[state])
	}
	inflight := 0
	if s.taskQueue != nil {
		inflight = s.taskQueue.InFlight()
	}
	fmt.Fprintf(&b, "# HELP srcos_queue_inflight Task runs currently in flight.\\n# TYPE srcos_queue_inflight gauge\\nsrcos_queue_inflight %d\\n", inflight)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, b.String())
}
