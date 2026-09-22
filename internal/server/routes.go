package server

import (
	"log"
	"strings"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file is the proxy layer's half of ADR-003/ADR-019's division of labour:
// the orchestration layer publishes endpoints, the proxy layer reads them. They
// meet in the routing table and nowhere else — nothing here starts, stops or
// inspects a container, and nothing in package runtime knows HTTP exists.
//
// The table is a *cache*, not a source of truth: instance records on disk are
// the truth (AGENTS.md: 文件系统即数据库), and `svc start` runs in a different
// process than the gateway. So the table is rebuilt from the records at startup
// and on every scan tick, and a lookup that misses does one targeted record
// load before giving up — which is what makes a service started a second ago
// reachable now, without polling faster than the scan loop.

// syncRoutes rebuilds the dynamic routing table from the instance records.
//
// Two directions matter equally: a live service must become reachable, and a
// service that stopped (or died) must stop being reachable. A stale entry is
// worse than a missing one: it keeps sending traffic to a loopback port the OS
// may have handed to something else entirely.
func (s *Server) syncRoutes() {
	if s.routes == nil {
		return
	}
	insts, err := runtime.ListInstances(config.DirOf(s.registry))
	if err != nil {
		log.Printf("[srcos] routes: %v", err)
		return
	}

	live := map[string]bool{}
	for _, inst := range insts {
		e, ok := routeEntryFrom(inst)
		if !ok {
			continue
		}
		if err := s.routes.Put(e); err != nil {
			// A record we cannot route is a configuration problem, not a
			// request problem: report it and keep serving the rest.
			log.Printf("[srcos] routes: instance %s: %v", inst.ID, err)
			continue
		}
		live[route.Key(e.User, e.Tool)] = true
	}

	// Drop entries whose instance is gone or no longer serving. Deleting by
	// instance id (not by key) keeps the ownership fence: a successor's route
	// is never removed by the reaper of its predecessor.
	for _, e := range s.routes.List() {
		if !live[route.Key(e.User, e.Tool)] {
			s.routes.DeleteInstance(e.User, e.Tool, e.InstanceID)
		}
	}
}

// routeEntryFrom renders an instance record as a route, or reports that it is
// not routable (a task, or a service that is not serving).
//
// The frontend path is *derived* (user + tool), never read from the record: a
// hand-edited file must not be able to move a service to another user's URL
// space, which is where the route cookies and the <base> rewriting point.
func routeEntryFrom(inst *runtime.Instance) (route.Entry, bool) {
	if inst.Kind != string(tool.KindService) {
		return route.Entry{}, false
	}
	if inst.State != runtime.StateRunning && inst.State != runtime.StateIdle {
		return route.Entry{}, false
	}
	target, err := route.ParseTarget(inst.Endpoint)
	if err != nil {
		return route.Entry{}, false
	}
	return route.Entry{
		User:       inst.User,
		Tool:       inst.Tool,
		InstanceID: inst.ID,
		Path:       route.DefaultPath(inst.User, inst.Tool),
		Target:     target,
		// Service instances are proxied with WebSocket support: a notebook or
		// a terminal in the browser is the normal case, not the exception.
		WebSocket: true,
		State:     string(inst.State),
	}, true
}

// publishInstanceRoute loads one instance record and publishes its route.
//
// This is the cache-miss path: `srcos svc start` runs in another process, so
// the gateway learns about a new service either from the scan tick or from
// here. One targeted load (a stat plus a small YAML parse) is cheaper than
// scanning on every request, and it removes the "I just started it and the link
// 404s" window entirely.
func (s *Server) publishInstanceRoute(user, toolID string) (route.Entry, bool) {
	inst, err := runtime.LoadInstance(runtime.InstancePath(config.DirOf(s.registry), runtime.InstanceID(user, toolID, "")))
	if err != nil {
		return route.Entry{}, false
	}
	e, ok := routeEntryFrom(inst)
	if !ok {
		return route.Entry{}, false
	}
	if err := s.routes.Put(e); err != nil {
		log.Printf("[srcos] routes: instance %s: %v", inst.ID, err)
		return route.Entry{}, false
	}
	return e, true
}

// instanceMatch resolves an instance-backed service from a /proxy/<user>/<tool>
// path, returning the static-card shape the rest of the proxy pipeline takes.
//
// A path naming another user never matches: the table is keyed by (user, tool)
// and the request's user segment must be the authenticated one.
func (s *Server) instanceMatch(requestPath, sessionUser string) *config.ServiceMatch {
	if s.routes == nil {
		return nil
	}
	user := config.UsernameFromProxyPath(requestPath)
	if user == "" || user != sessionUser {
		return nil
	}
	rest := strings.TrimPrefix(requestPath, "/proxy/"+user+"/")
	if rest == "" {
		return nil
	}
	toolID := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		toolID = rest[:i]
	}
	if toolID == "" {
		return nil
	}

	e, ok := s.routes.Get(user, toolID)
	if !ok {
		e, ok = s.publishInstanceRoute(user, toolID)
		if !ok {
			return nil
		}
	}

	remaining := rest[len(toolID):]
	if remaining == "" {
		remaining = "/"
	}
	return &config.ServiceMatch{
		Username:      user,
		Service:       serviceForEntry(e),
		RemainingPath: remaining,
	}
}

// serviceForEntry renders a route in the shape the forwarding pipeline takes.
//
// Instance-backed and card-backed services therefore share one path through the
// proxy: base rewriting, route cookies, WebSocket support and bandwidth limits
// are written once and apply to both.
func serviceForEntry(e route.Entry) *config.ServiceConfig {
	return &config.ServiceConfig{
		ID:          e.Tool,
		Name:        e.Tool,
		Host:        e.Target.Host,
		Port:        e.Target.Port,
		Path:        strings.TrimPrefix(e.Path, "/proxy/"+e.User),
		WebSocket:   e.WebSocket,
		BWLimit:     e.BWLimit,
		BackendPath: e.BackendPath,
	}
}

// dropDeadRoute removes a route whose target just refused a connection.
//
// The table is a cache of the records, so a service stopped by another process
// stays in it until the next scan tick — and every request in between would
// dial a port that is gone. Dropping the entry on the failure turns that into
// one 502 followed by the truth (the record says stopped, so the next request
// is a 404), while a service that is merely restarting is republished on the
// next miss, because its record still says running.
func (s *Server) dropDeadRoute(username string, svc *config.ServiceConfig) {
	if s.routes == nil || username == "" || svc == nil {
		return
	}
	e, ok := s.routes.Get(username, svc.ID)
	if !ok {
		return // a static card, or already dropped
	}
	// Only drop the entry we actually tried to dial: a successor instance on a
	// new port must keep its route.
	if e.Target.Host != svc.Host || e.Target.Port != svc.Port {
		return
	}
	if s.routes.DeleteInstance(e.User, e.Tool, e.InstanceID) {
		log.Printf("[srcos] routes: dropped %s (%s) after a failed dial: the instance is gone or stopping",
			e.InstanceID, e.Path)
	}
}

// barePathRoute resolves a bare gateway path (e.g. /websvc/index.html) to an
// instance-backed service, so an instance is reachable by its own short URL the
// same way a static card is. It returns the canonical /proxy/... redirect
// target.
//
// This is the instance half of redirectBareService: a user should not have to
// remember which kind of service they are looking at.
func (s *Server) barePathRoute(requestPath, username string) (string, bool) {
	if s.routes == nil {
		return "", false
	}
	rest := strings.TrimPrefix(requestPath, "/")
	if rest == "" {
		return "", false
	}
	toolID := rest
	tail := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		toolID, tail = rest[:i], rest[i:]
	}
	if toolID == "" {
		return "", false
	}
	if _, ok := s.routes.Get(username, toolID); !ok {
		if _, ok := s.publishInstanceRoute(username, toolID); !ok {
			return "", false
		}
	}
	return route.DefaultPath(username, toolID) + tail, true
}

// matchRouteForUser maps a path (a Referer, or a route cookie) to the backend
// that should serve it: a live instance first, then a static card.
//
// Instances come first because they are live, while a card is a hand-written
// pointer that may name a port nothing listens on any more. That is also what
// package route documents ("the proxy reads it before falling back to the
// static card list").
func (s *Server) matchRouteForUser(path, username string) *config.ServiceMatch {
	if m := s.instanceMatch(path, username); m != nil {
		return m
	}
	match := s.registry.FindService(path)
	if match == nil {
		match = s.registry.FindLegacyService(path, username)
	}
	if match == nil {
		return nil
	}
	if pathUser := config.UsernameFromProxyPath(path); pathUser != "" && pathUser != username {
		return nil
	}
	return match
}
