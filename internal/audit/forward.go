package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sink delivers events somewhere the platform's own disk cannot rewrite.
//
// This is what makes the hash chain mean something: a local chain proves a file
// was not edited in place, but whoever can rewrite every file can recompute it.
// The anchor has to live outside the write domain — another machine, a log
// collector, a SIEM. For that reason SRCOS does **not** ship local signing: an
// HMAC key kept on the same host buys almost nothing and looks like more than
// it is.
type Sink interface {
	// Name is what shows up in the admin page ("http").
	Name() string
	// Send delivers one event. An error means "keep it and try again": the
	// forwarder must not lose an event because a collector was restarting.
	Send(ctx context.Context, e Event) error
}

// HTTPSink POSTs one event as JSON to a collector.
//
// One request per event is deliberate: the volume is human actions and
// lifecycle decisions, not request logs, and a batch protocol would need
// retry/idempotency semantics the collector may not have.
type HTTPSink struct {
	URL   string
	Token string
	// Client is the HTTP client to use. Nil builds one with a 5s timeout.
	Client *http.Client
}

func (s *HTTPSink) Name() string { return "http" }

func (s *HTTPSink) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("collector answered %s", resp.Status)
	}
	return nil
}

// Forwarder spools events to disk and drains them to a Sink.
//
// The spool is one file per event. That is the simplest shape that has no
// partial-write problem (a batch file being appended to cannot be deleted
// safely, and tracking a read cursor needs durable offsets), and the volume is
// low enough that the inode cost does not matter. Delivery is at-least-once: an
// event is deleted only after the sink accepted it.
type Forwarder struct {
	dir  string
	sink Sink
	// max bounds the spool: past it the oldest events are dropped and counted.
	// An unbounded spool is a disk-full outage waiting to happen; a *silent*
	// drop hides an outage that the operator has to know about.
	max int

	mu      sync.Mutex
	dropped int
}

// DefaultForwardMax is the spool ceiling (events) when none is configured.
const DefaultForwardMax = 100_000

// NewForwarder builds a forwarder spooling under spoolDir.
func NewForwarder(spoolDir string, sink Sink, max int) *Forwarder {
	if max <= 0 {
		max = DefaultForwardMax
	}
	return &Forwarder{dir: spoolDir, sink: sink, max: max}
}

// Dir is where this forwarder spools.
func (f *Forwarder) Dir() string {
	if f == nil {
		return ""
	}
	return f.dir
}

// Spool stores one event for later delivery. It is local and fast; it must not
// talk to the network (the request path calls it).
func (f *Forwarder) Spool(e Event) error {
	if f == nil || f.sink == nil {
		return nil
	}
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%s.json", e.TS.UnixNano(), randSuffix())
	if err := os.WriteFile(filepath.Join(f.dir, name), append(body, '\n'), 0o600); err != nil {
		return err
	}
	return f.enforceCeiling()
}

// randSuffix makes two events in the same nanosecond distinct file names. The
// timestamp prefix still orders them; this only breaks ties.
func randSuffix() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b[:])
}

// enforceCeiling drops the oldest spooled events past the configured maximum.
func (f *Forwarder) enforceCeiling() error {
	files, err := f.spooled()
	if err != nil {
		return err
	}
	if len(files) <= f.max {
		return nil
	}
	drop := len(files) - f.max
	for _, path := range files[:drop] {
		if err := os.Remove(path); err == nil {
			f.mu.Lock()
			f.dropped++
			f.mu.Unlock()
		}
	}
	return nil
}

// Run drains the spool until ctx is done. A failing sink leaves the spool
// intact and is retried at the next tick; the caller decides the interval.
func (f *Forwarder) Run(ctx context.Context, interval time.Duration) {
	if f == nil || f.sink == nil {
		return
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		f.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// drain sends as many spooled events as the sink accepts, oldest first. It stops
// at the first failure: order is part of the record, and skipping past a failing
// event would deliver a stream with a hole in it.
func (f *Forwarder) drain(ctx context.Context) {
	files, err := f.spooled()
	if err != nil {
		return
	}
	for _, path := range files {
		if ctx.Err() != nil {
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var e Event
		if err := json.Unmarshal(bytes.TrimSpace(data), &e); err != nil {
			// Unreadable spool entry: it can never be delivered, so keeping it
			// would block the queue forever. Drop it, and record one local
			// event saying so.
			_ = os.Remove(path)
			continue
		}
		if err := f.sink.Send(ctx, e); err != nil {
			return
		}
		if err := os.Remove(path); err != nil {
			return
		}
	}
}

// spooled lists the spool files oldest first (the names start with the event's
// timestamp in nanoseconds, so lexical order is chronological).
func (f *Forwarder) spooled() ([]string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		files = append(files, filepath.Join(f.dir, e.Name()))
	}
	sort.Strings(files)
	return files, nil
}

// Stats reports how much is waiting and how much was dropped, for the admin
// page. An operator has to be able to see "the collector is down and N events
// are queued" — silence is the one thing this must not do.
func (f *Forwarder) Stats() (unsent, dropped int) {
	if f == nil || f.sink == nil {
		return 0, 0
	}
	files, err := f.spooled()
	if err == nil {
		unsent = len(files)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return unsent, f.dropped
}

// SinkName is the configured sink, for display.
func (f *Forwarder) SinkName() string {
	if f == nil || f.sink == nil {
		return ""
	}
	return f.sink.Name()
}
