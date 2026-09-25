package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestForwarderDeliversAndClears(t *testing.T) {
	var mu sync.Mutex
	var got []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	f := NewForwarder(t.TempDir(), &HTTPSink{URL: srv.URL}, 0)
	for i := 0; i < 3; i++ {
		e := NewEvent(Actor{User: "alice"}, "submit").Allowed()
		e.TS = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := f.Spool(e); err != nil {
			t.Fatal(err)
		}
	}
	if unsent, dropped := f.Stats(); unsent != 3 || dropped != 0 {
		t.Fatalf("before drain: unsent=%d dropped=%d, want 3/0", unsent, dropped)
	}

	f.drain(context.Background())

	if unsent, dropped := f.Stats(); unsent != 0 || dropped != 0 {
		t.Fatalf("after drain: unsent=%d dropped=%d, want 0/0", unsent, dropped)
	}
	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 3 {
		t.Fatalf("collector received %d events, want 3", n)
	}
}

// A collector that is down must not lose anything: the spool is the buffer.
func TestForwarderKeepsEventsWhenSinkFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	f := NewForwarder(t.TempDir(), &HTTPSink{URL: srv.URL}, 0)
	for i := 0; i < 2; i++ {
		e := NewEvent(Actor{User: "alice"}, "submit").Allowed()
		e.TS = time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		if err := f.Spool(e); err != nil {
			t.Fatal(err)
		}
	}
	f.drain(context.Background())

	if unsent, _ := f.Stats(); unsent != 2 {
		t.Fatalf("a failing collector must not drop events: unsent=%d, want 2", unsent)
	}
}

// Past the ceiling the oldest go, and the drop is counted — never silent.
func TestForwarderCeilingDropsOldestAndCounts(t *testing.T) {
	f := NewForwarder(t.TempDir(), &HTTPSink{URL: "http://127.0.0.1:1"}, 2)
	for i := 0; i < 3; i++ {
		e := NewEvent(Actor{User: "alice"}, "submit").Allowed()
		e.TS = time.Now().UTC().Add(time.Duration(i) * time.Second)
		if err := f.Spool(e); err != nil {
			t.Fatal(err)
		}
	}
	unsent, dropped := f.Stats()
	if unsent != 2 || dropped != 1 {
		t.Fatalf("unsent=%d dropped=%d, want 2/1", unsent, dropped)
	}
}

func TestRecorderSpoolsToAttachedForwarder(t *testing.T) {
	dir := t.TempDir()
	f := NewForwarder(filepath.Join(dir, "spool"), &HTTPSink{URL: "http://127.0.0.1:1"}, 0)
	r := New(dir)
	r.ForwardTo(f)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	r.Close()

	if unsent, _ := f.Stats(); unsent != 1 {
		t.Fatalf("the recorder must spool when a forwarder is attached: unsent=%d", unsent)
	}
	st := r.ForwardStatus()
	if !st.Configured || st.Sink != "http" || st.Unsent != 1 {
		t.Fatalf("ForwardStatus = %+v", st)
	}

	// Detaching stops the spooling.
	r.ForwardTo(nil)
	r.Record(NewEvent(Actor{User: "alice"}, "submit").Allowed())
	if unsent, _ := f.Stats(); unsent != 1 {
		t.Fatalf("detached forwarder should not grow: unsent=%d", unsent)
	}
	if st := r.ForwardStatus(); st.Configured {
		t.Fatalf("status should report nothing configured after detach: %+v", st)
	}
}

func TestForwarderNilIsSafe(t *testing.T) {
	var f *Forwarder
	if err := f.Spool(NewEvent(Actor{User: "a"}, "submit").Allowed()); err != nil {
		t.Fatalf("nil Spool: %v", err)
	}
	if unsent, dropped := f.Stats(); unsent != 0 || dropped != 0 {
		t.Fatalf("nil Stats = %d/%d", unsent, dropped)
	}
	f.Run(context.Background(), time.Millisecond) // must return (no sink)
	if f.SinkName() != "" {
		t.Fatal("nil SinkName should be empty")
	}
}
