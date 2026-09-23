package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
)

// writeInstance drops an instance record (and its log file) into the harness's
// config directory, the way the runtime would.
func writeInstance(t *testing.T, h *toolsHarness, id string, state runtime.State, log string) string {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), id+".log")
	if err := os.WriteFile(logPath, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := &runtime.Instance{
		ID:        id,
		User:      "alice",
		Tool:      "demo",
		Kind:      "task",
		JobName:   id,
		State:     state,
		LogPath:   logPath,
		StartedAt: time.Now().UTC(),
	}
	if err := runtime.SaveInstance(runtime.InstancePath(h.configDir, id), inst); err != nil {
		t.Fatal(err)
	}
	return logPath
}

func TestLogsPlainTail(t *testing.T) {
	h := newToolsHarness(t, nil)
	writeInstance(t, h, "alice-demo-job1", runtime.StateRunning, "a\nb\nc\n")

	rec := h.get(t, "/api/jobs/alice-demo-job1/logs?tail=2")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	if got := rec.Body.String(); !strings.HasSuffix(got, "b\nc\n") {
		t.Errorf("body = %q, want it to end with the last two lines", got)
	}
}

func TestLogsRefusesUnknownAndMalformed(t *testing.T) {
	h := newToolsHarness(t, nil)
	writeInstance(t, h, "alice-demo-job1", runtime.StateRunning, "x\n")

	cases := []struct {
		url  string
		want int
	}{
		{"/api/jobs/alice-demo-nope/logs", 404},
		{"/api/jobs/alice-demo-job1/other", 404}, // the shape is fixed
		{"/api/jobs//logs", 404},
		{"/api/jobs/..%2F..%2Fetc/logs", 404},
	}
	for _, c := range cases {
		rec := h.get(t, c.url)
		if rec.Code != c.want {
			t.Errorf("%s = %d, want %d (%s)", c.url, rec.Code, c.want, rec.Body)
		}
	}
}

func TestLogsSSEStreamReplaysAndEnds(t *testing.T) {
	h := newToolsHarness(t, nil)
	// A terminal instance keeps the test fast: the stream replays the tail and
	// closes, which exercises the framing without waiting a poll interval. The
	// live path is covered by the inspect tests and the browser e2e.
	writeInstance(t, h, "alice-demo-job1", runtime.StateSucceeded, "one\ntwo\n")

	rec := h.get(t, "/api/jobs/alice-demo-job1/logs?follow=1")
	if rec.Code != 200 {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: line\ndata: one\n\n",
		"event: line\ndata: two\n\n",
		"event: state\ndata: succeeded\n\n",
		"event: end\ndata: \n\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream is missing %q:\n%s", want, body)
		}
	}
	// The end event must be last, or a client would keep listening.
	if i := strings.Index(body, "event: end"); i < 0 || strings.TrimSpace(body[i+len("event: end"):]) != "data:" {
		t.Errorf("the end event is not last:\n%s", body)
	}
}

func TestLogsSSEFollowOnANewlinesPayload(t *testing.T) {
	h := newToolsHarness(t, nil)
	// A line containing a carriage return is folded to what a terminal shows;
	// a payload with a newline would have to be split across `data:` fields,
	// which the framing helper does.
	writeInstance(t, h, "alice-demo-job1", runtime.StateSucceeded, "50%\r100%\n")

	rec := h.get(t, "/api/jobs/alice-demo-job1/logs?follow=1")
	if !strings.Contains(rec.Body.String(), "data: 100%") {
		t.Errorf("body = %q", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "\r") {
		t.Errorf("a bare carriage return reached the stream: %q", rec.Body)
	}
}
