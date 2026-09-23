package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
)

// This file serves an instance's log at two speeds.
//
//	GET /api/jobs/<id>/logs              → the tail, as plain text
//	GET /api/jobs/<id>/logs?follow=1     → Server-Sent Events, live
//
// The one-shot form is the honest default: it is what `curl` wants, what a
// no-JavaScript page shows, and what an agent reads. SSE is opt-in because a
// stream is a different contract — it never ends on its own, and a client that
// did not ask for one should not have to know to hang up.
//
// The events are named (`line` / `state` / `note` / `end`) rather than raw
// `message` frames: a client that only understands "append this line" can listen
// for `line` alone and ignore the rest.

// handleLogs serves one instance's log.
func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request, username, id string) {
	reader := h.reader()
	tail := inspect.DefaultLogTail
	if raw := strings.TrimSpace(r.URL.Query().Get("tail")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			tail = n
		}
	}

	if r.URL.Query().Get("follow") != "1" {
		text, err := reader.Logs(username, id, tail)
		if err != nil {
			writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, text)
		return
	}

	// Resolve before switching protocols: a bad or ambiguous id should be a
	// 404/400 answer, not a stream that opens and immediately ends (which a
	// browser turns into a silent reconnect loop).
	if _, _, err := reader.Instance(username, id); err != nil {
		writeJSON(w, ErrorStatus(err), map[string]string{"error": err.Error()})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "this server cannot stream"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// A reverse proxy (nginx, a tunnel) must not buffer the tail; without this
	// the stream would arrive in chunks whenever the proxy felt like flushing.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	err := reader.FollowLogs(r.Context(), username, id, tail, func(ev inspect.LogEvent) {
		writeLogEvent(w, ev)
		flusher.Flush()
	})
	if err != nil {
		// The headers are out, so the only channel left is the stream itself.
		// (FollowLogs only fails before its first event, so this is rare.)
		sseEvent(w, "note", err.Error())
		sseEvent(w, "end", "")
		flusher.Flush()
	}
}

// writeLogEvent maps one follower event onto an SSE frame.
func writeLogEvent(w io.Writer, ev inspect.LogEvent) {
	switch ev.Kind {
	case inspect.LogEventLine:
		sseEvent(w, "line", ev.Line)
	case inspect.LogEventPing:
		// A comment is the SSE way to say "still here": it carries no event.
		_, _ = io.WriteString(w, ": ping\n\n")
	case inspect.LogEventEnd:
		if ev.State != "" {
			sseEvent(w, "state", ev.State)
		}
		if ev.Note != "" {
			sseEvent(w, "note", ev.Note)
		}
		sseEvent(w, "end", "")
	}
}

// jobLogID extracts the instance id from /api/jobs/<id>/logs.
//
// The shape is fixed rather than generic: `/api/jobs/<id>` alone is not an
// endpoint, so a caller is never left guessing whether it read a log or
// something else. A separator in the id is refused (instance ids are flat),
// which also keeps `..` from ever reaching the store.
func jobLogID(path string) (string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/jobs/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(rest, "/logs")
	if !ok || id == "" {
		return "", false
	}
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") {
		return "", false
	}
	return id, true
}

// sseEvent writes one frame. Newlines in the payload are folded into repeated
// `data:` fields, which is what the event-stream grammar requires (a bare
// newline would end the frame).
func sseEvent(w io.Writer, event, data string) {
	data = strings.ReplaceAll(data, "\r", "")
	if !strings.Contains(data, "\n") {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "event: %s\n", event)
	for _, line := range strings.Split(data, "\n") {
		fmt.Fprintf(&b, "data: %s\n", line)
	}
	b.WriteString("\n")
	_, _ = io.WriteString(w, b.String())
}
