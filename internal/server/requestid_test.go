package server

import (
	"bufio"
	"bytes"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDEchoesAValidIDAndReplacesAnInvalidOne(t *testing.T) {
	h := requestIDHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set(requestIDHeader, "trace-abc_1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(requestIDHeader); got != "trace-abc_1" {
		t.Fatalf("valid id = %q", got)
	}

	req = httptest.NewRequest("GET", "/api/tools", nil)
	req.Header.Set(requestIDHeader, "bad id!")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get(requestIDHeader); got == "bad id!" || got == "" {
		t.Fatalf("invalid id was not replaced: %q", got)
	}
}

type fakeStreamWriter struct {
	http.ResponseWriter
	flushed  bool
	hijacked bool
}

func (f *fakeStreamWriter) Flush() { f.flushed = true }
func (f *fakeStreamWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	f.hijacked = true
	return nil, nil, nil
}

func TestRequestIDWriterKeepsFlusherAndHijacker(t *testing.T) {
	fake := &fakeStreamWriter{ResponseWriter: httptest.NewRecorder()}
	h := requestIDHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("the request writer lost http.Flusher")
		}
		f.Flush()
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("the request writer lost http.Hijacker")
		}
		if _, _, err := hj.Hijack(); err != nil {
			t.Fatal(err)
		}
	}))
	h.ServeHTTP(fake, httptest.NewRequest("GET", "/api/jobs/x/logs?follow=1", nil))
	if !fake.flushed || !fake.hijacked {
		t.Fatalf("flushed=%v hijacked=%v", fake.flushed, fake.hijacked)
	}
}

func TestRequestIDLogsOnlyGatewayPaths(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	h := requestIDHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/tools", nil))
	if !strings.Contains(buf.String(), "request_id=") {
		t.Fatalf("gateway request was not logged: %s", buf.String())
	}
	buf.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/-/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/proxy/alice/demo/", nil))
	if buf.Len() != 0 {
		t.Fatalf("probe/proxy request was logged: %s", buf.String())
	}
}
