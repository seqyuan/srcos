package server

import (
	"context"
	"log"
	"time"
)

// This file is the gateway's background loops: the config/routing scan tick and
// the task-queue drain loop. Both are started by runServer and stopped on
// shutdown.

// ScanLoop periodically reloads the user registry and enforces the lifecycle
// ceilings.
//
// The task queue has its own loop (TaskLoop): a submission wakes it, and it is
// the thing that makes "目录即队列" true without a human typing `srcos job run`.
func (s *Server) ScanLoop(interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			before := len(s.registry.ListUsers())
			s.registry.Reload()
			after := len(s.registry.ListUsers())
			// Only log when the registry actually changed to avoid log spam.
			if after != before {
				log.Printf("[srcos] user registry changed: %d -> %d user(s)", before, after)
			}
			// Keep the routing table in step with the instance records: a
			// service started or stopped by the CLI (another process) becomes
			// reachable or unreachable within one tick.
			s.syncRoutes()
			// A hand-edited grants.yaml takes effect here, without a restart.
			s.reloadPolicyIfChanged()
			// Enforce the lifecycle ceilings (idleTTL / maxLifetime). Idle
			// means "no traffic", which is why the reaper is given the proxy's
			// observations rather than the record's start time.
			s.reapServices()
			// Push out the scheduler's own ceiling on long-running services
			// (`qalter -l h_rt=...`) before it kills work the platform wants.
			s.renewServices()
			// Settle task records whose process died while the gateway was away:
			// a task's verdict is written by the process that started it, and a
			// "running" record that nothing backs is a lie that also holds the
			// user's quota.
			s.reconcileTasks()
			// The same for services, whose liveness was previously only checked
			// at startup: a service that dies later must not keep saying
			// "running" (and keep a route to a dead port) until a lifecycle
			// ceiling happens to fire.
			s.reconcileServices()
		case <-stop:
			return
		}
	}
}

// TaskLoop drains the task drop-box until ctx ends (ADR-004's missing
// consumer): every submission runs without anyone typing `srcos job run`.
//
// It is a no-op on a deployment that disabled the queue, and it starts with an
// immediate pass, so work submitted while SRCOS was down runs as soon as it is
// back.
func (s *Server) TaskLoop(ctx context.Context, interval time.Duration) {
	if s.taskQueue == nil {
		return
	}
	s.taskQueue.Run(ctx, interval)
}
