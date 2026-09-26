package execute

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/audit"
	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/flow"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// ── a fake backend ───────────────────────────────────────────────────────
//
// The write path's job is to decide *whether* and *what* to run; a real
// backend would spawn processes, which a unit test must not do. The fake
// records what was started and lets the test decide when it finishes, so
// "running" and "finished" are both observable states.

type fakeBackend struct {
	mu      sync.Mutex
	handles []*fakeHandle
	// auto finishes every unit the moment it starts. Tests that need to observe
	// a *running* instance leave it false and release explicitly; a flow test
	// turns it on, because a multi-node flow starts its later jobs only after
	// the earlier ones finish.
	auto atomic.Bool
}

func (b *fakeBackend) Name() string { return "local" }

func (b *fakeBackend) Start(ctx context.Context, req runtime.StartRequest) (runtime.Handle, error) {
	// The real backend creates the log; the record points at it, so the file
	// must exist for the read side to find anything.
	if req.LogPath != "" {
		_ = os.WriteFile(req.LogPath, []byte("fake output\n"), 0o644)
	}
	h := &fakeHandle{done: make(chan struct{}), command: req.Argv}
	if b.auto.Load() {
		// Through Stop, so the once guard also covers this close.
		_ = h.Stop(ctx)
	}
	b.mu.Lock()
	b.handles = append(b.handles, h)
	b.mu.Unlock()
	return h, nil
}

// finishAll lets every started unit complete with exit code 0 — and every unit
// started later, so a run still being *prepared* when the test releases does not
// block forever (a reservation exists before its process does).
func (b *fakeBackend) finishAll() {
	b.auto.Store(true)
	b.mu.Lock()
	handles := append([]*fakeHandle(nil), b.handles...)
	b.mu.Unlock()
	for _, h := range handles {
		h.Stop(context.Background())
	}
}

// live counts units that have started and not finished.
func (b *fakeBackend) live() int {
	b.mu.Lock()
	handles := append([]*fakeHandle(nil), b.handles...)
	b.mu.Unlock()
	n := 0
	for _, h := range handles {
		if !h.doneClosed() {
			n++
		}
	}
	return n
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.handles)
}

type fakeHandle struct {
	done    chan struct{}
	once    sync.Once
	command []string
}

func (h *fakeHandle) Ref() string                    { return "fake-unit" }
func (h *fakeHandle) Command() []string              { return h.command }
func (h *fakeHandle) Limiter() string                { return "fake" }
func (h *fakeHandle) WantEndpoint() bool             { return false }
func (h *fakeHandle) Endpoint() (route.Target, bool) { return route.Target{}, false }

func (h *fakeHandle) Wait(ctx context.Context) runtime.ExitStatus {
	select {
	case <-h.done:
		return runtime.ExitStatus{Code: 0}
	case <-ctx.Done():
		return runtime.ExitStatus{Code: -1, Err: ctx.Err()}
	}
}

// doneClosed reports whether this unit has finished.
func (h *fakeHandle) doneClosed() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

func (h *fakeHandle) Alive() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

func (h *fakeHandle) Stop(ctx context.Context) error {
	h.once.Do(func() { close(h.done) })
	return nil
}

// ── fixture ──────────────────────────────────────────────────────────────

const demoManifest = `
schemaVersion: 1
id: demo
version: 0.1.0
name: Demo
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: word, type: string, required: true}
  outputs:
    - {name: out, type: directory}
resources: {cpu: 1, memory: "1Gi", walltime: "0:10:00"}
`

const otherManifest = `
schemaVersion: 1
id: other
version: 0.2.0
name: Other
kind: task
backend: local
sandbox: none
entry: work.sh
interface:
  inputs:
    - {name: word, type: string, required: true}
resources: {cpu: 1, memory: "1Gi", walltime: "0:10:00"}
`

const svcManifest = `
schemaVersion: 1
id: web
version: 0.1.0
name: Web
kind: service
backend: local
sandbox: none
entry: work.sh
resources: {cpu: 1, memory: "1Gi"}
ingress: {port: 8080}
lifecycle: {max_lifetime: "1h"}
`

type fixture struct {
	*Controller
	configDir string
	toolsDir  string
	flowsDir  string
	backend   *fakeBackend
	queue     *Queue
}

func newFixture(t *testing.T, policy *grant.Policy) *fixture {
	t.Helper()
	configDir := t.TempDir()
	writeUser(t, configDir, "alice")
	writeUser(t, configDir, "bob")

	toolsDir := t.TempDir()
	for id, manifest := range map[string]string{"demo": demoManifest, "other": otherManifest, "web": svcManifest} {
		dir := filepath.Join(toolsDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool.yaml"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "work.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	flowsDir := t.TempDir()
	flowYAML := `schemaVersion: 1
id: pipe
version: 0.1.0
name: Pipe
nodes:
  - {id: first, tool: demo}
  - {id: second, tool: other, depends_on: [first]}
bindings: []
expose:
  - {node: first, input: word, from: sample.word}
  - {node: second, input: word, from: sample.word}
`
	// The flow package's layout is <dir>/<id>/flow.yaml.
	if err := os.MkdirAll(filepath.Join(flowsDir, "pipe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flowsDir, "pipe", "flow.yaml"), []byte(flowYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	fb := &fakeBackend{}
	newRunner := func(user string) (*runtime.Runner, error) {
		return runtime.NewRunner(runtime.Options{
			ConfigDir: configDir,
			ToolsDir:  toolsDir,
			User:      user,
			Backends:  map[string]runtime.Backend{"local": fb},
		}), nil
	}
	c := New(Options{
		ConfigDir: configDir,
		ToolsDir:  toolsDir,
		FlowsDir:  flowsDir,
		Grants:    policy,
		NewRunner: newRunner,
	})
	q := NewQueue(c, QueueOptions{
		Workers: 2,
		Users:   func() []string { return []string{"alice", "bob"} },
		Log:     func(string, ...any) {}, // a test's queue is quiet
	})
	return &fixture{
		Controller: c, configDir: configDir, toolsDir: toolsDir,
		flowsDir: flowsDir, backend: fb, queue: q,
	}
}

// drain runs one pass of the task queue: the daemon's tick, synchronously.
func (f *fixture) drain(t *testing.T) int {
	t.Helper()
	return f.queue.Tick(context.Background())
}

func writeUser(t *testing.T, configDir, user string) {
	t.Helper()
	path := config.UserConfigPath(configDir, user)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "auth:\n  password_hash: \"" + strings.Repeat("1", 64) + "\"\nservices: []\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// allowAll grants every tool publicly.
func allowAll(t *testing.T) *grant.Policy {
	t.Helper()
	p, err := grant.New(nil, nil, []grant.Grant{{Tool: "*", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// tokenIdentity mints a real token and verifies it, so the tests exercise the
// same path a request does.
func tokenIdentity(t *testing.T, user string, scopes []agenttoken.Scope, submitTools []string) agenttoken.Identity {
	t.Helper()
	store := agenttoken.New(filepath.Join(t.TempDir(), "agent-tokens.yaml"))
	_, raw, err := store.Create(agenttoken.CreateParams{User: user, Scopes: scopes, SubmitTools: submitTools})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	ident, err := store.Authenticate(req)
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

// ── submit ───────────────────────────────────────────────────────────────

func TestSubmitValidation(t *testing.T) {
	f := newFixture(t, allowAll(t))
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	cases := []struct {
		name string
		req  SubmitRequest
		want error
	}{
		{"no tool", SubmitRequest{}, ErrBadRequest},
		{"unknown tool", SubmitRequest{Tool: "nope", Params: map[string]any{"word": "x"}}, ErrNotFound},
		{"service tool", SubmitRequest{Tool: "web"}, ErrBadRequest},
		{"missing required param", SubmitRequest{Tool: "demo"}, ErrBadRequest},
		{"unknown param", SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x", "extra": 1}}, ErrBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := f.Submit(ident, c.req); !errors.Is(err, c.want) {
				t.Fatalf("error = %v, want %v", err, c.want)
			}
		})
	}
}

func TestSubmitHonoursGrantAndAllowlist(t *testing.T) {
	// "demo" is public, "other" is not granted to alice.
	policy, err := grant.New(nil, nil, []grant.Grant{{Tool: "demo", Public: true}})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, policy)

	open := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	// An ungranted tool is "not found": the write side does not confirm that a
	// tool exists for someone who may not use it.
	if _, err := f.Submit(open, SubmitRequest{Tool: "other", Params: map[string]any{"word": "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted tool = %v, want ErrNotFound", err)
	}
	// Granted, but outside the credential's allowlist.
	narrowed := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, []string{"other"})
	if _, err := f.Submit(narrowed, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("allowlist miss = %v, want ErrForbidden", err)
	}
	// Same credential, the allowlisted tool (which is still ungranted).
	if _, err := f.Submit(narrowed, SubmitRequest{Tool: "other", Params: map[string]any{"word": "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("allowlist hit but ungranted = %v, want ErrNotFound", err)
	}
	// A read-only token cannot submit at all.
	readOnly := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeRead}, nil)
	if _, err := f.Submit(readOnly, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only token = %v, want ErrForbidden", err)
	}
}

// A submission is a drop-box entry (ADR-004) plus a *pending* record, so the
// id is addressable before anything runs — and the queue, not the request, is
// what starts it.
func TestSubmitQueuesAndIsAddressable(t *testing.T) {
	f := newFixture(t, allowAll(t))
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	res, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.JobID == "" || res.InstanceID == "" || res.State != string(runtime.StatePending) {
		t.Fatalf("result = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "job.json")); err != nil {
		t.Fatalf("job.json was not written: %v", err)
	}
	// Nothing has started yet: the request queued the work.
	if f.backend.count() != 0 {
		t.Fatalf("submit started %d run(s) itself", f.backend.count())
	}
	rec, err := runtime.LoadInstance(runtime.InstancePath(f.configDir, res.InstanceID))
	if err != nil {
		t.Fatalf("the queued instance must be addressable: %v", err)
	}
	if rec.State != runtime.StatePending {
		t.Fatalf("record = %+v, want pending", rec)
	}

	// The queue runs it.
	if n := f.drain(t); n != 1 {
		t.Fatalf("the queue started %d run(s), want 1", n)
	}
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateRunning)
	f.backend.finishAll()
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateSucceeded)

	// A second pass must not run it again.
	if n := f.drain(t); n != 0 {
		t.Fatalf("the queue re-started a finished job (%d)", n)
	}
}

// The quota bounds *simultaneous* consumption, not queue length: a user may
// submit more than they can run at once (that is what a queue is for), and the
// drain starts the next one only when the ceiling has room.
func TestQuotaGatesStartsNotSubmissions(t *testing.T) {
	policy, err := grant.New(nil, []string{"alice"}, []grant.Grant{
		{Tool: "demo", Public: true, Quota: grant.Quota{MaxInstances: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, policy)
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	first, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "b"}})
	if err != nil {
		t.Fatalf("queuing a second run must not be refused: %v", err)
	}
	// Pin the submission order: two files written microseconds apart may share
	// an mtime, and the queue's tie-break is then arbitrary.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(first.Dir, "job.json"), past, past); err != nil {
		t.Fatal(err)
	}

	// One pass starts only one: the other is over the ceiling for now.
	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d, want 1 (max_instances 1)", n)
	}
	waitInstance(t, f.configDir, first.InstanceID, runtime.StateRunning)
	if rec, _ := runtime.LoadInstance(runtime.InstancePath(f.configDir, second.InstanceID)); rec.State != runtime.StatePending {
		t.Fatalf("second = %+v, want still queued", rec)
	}

	// Let the first finish; the ceiling frees and the second starts.
	f.backend.finishAll()
	waitInstance(t, f.configDir, first.InstanceID, runtime.StateSucceeded)
	if n := f.drain(t); n != 1 {
		t.Fatalf("queue started %d after the ceiling freed, want 1", n)
	}
	// The fake backend is in auto-finish mode by now, so the run completes on
	// its own; what matters is that it left the queue.
	waitInstance(t, f.configDir, second.InstanceID, runtime.StateSucceeded)
	f.releaseAndSettle(t)
}

// ── cancel ───────────────────────────────────────────────────────────────

func TestCancelStopsARunningInstance(t *testing.T) {
	f := newFixture(t, allowAll(t))
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	res, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateRunning)

	out, err := f.Cancel(context.Background(), ident, res.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	if out.InstanceID != res.InstanceID || out.State != string(runtime.StateStopped) {
		t.Fatalf("cancel result = %+v", out)
	}
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateStopped)

	// Cancelling again is the caller's intent already satisfied, not an error.
	again, err := f.Cancel(context.Background(), ident, res.InstanceID)
	if err != nil {
		t.Fatalf("second cancel = %v, want nil", err)
	}
	if again.State != string(runtime.StateStopped) {
		t.Fatalf("second cancel = %+v", again)
	}
}

func TestCancelScopeRules(t *testing.T) {
	f := newFixture(t, allowAll(t))
	owner := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	res, err := f.Submit(owner, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateRunning)

	// Another user cannot even see it.
	stranger := tokenIdentity(t, "bob", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	if _, err := f.Cancel(context.Background(), stranger, res.InstanceID); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user's cancel = %v, want ErrNotFound", err)
	}
	// A credential whose allowlist excludes the instance's tool cannot stop it.
	narrowed := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, []string{"other"})
	if _, err := f.Cancel(context.Background(), narrowed, res.InstanceID); !errors.Is(err, ErrForbidden) {
		t.Errorf("allowlist miss = %v, want ErrForbidden", err)
	}
	// A read-only token cannot cancel.
	readOnly := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeRead}, nil)
	if _, err := f.Cancel(context.Background(), readOnly, res.InstanceID); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-only cancel = %v, want ErrForbidden", err)
	}
	// An empty needle is a bad request, not a not-found.
	if _, err := f.Cancel(context.Background(), owner, "  "); !errors.Is(err, ErrBadRequest) {
		t.Errorf("empty needle = %v, want ErrBadRequest", err)
	}
	f.releaseAndSettle(t)
}

// A deliberate stop is written by a different goroutine than the one waiting
// for the process. The waiter must not overwrite it: without the guard in
// RunTask, a cancelled run ends up "failed (signal: terminated)" — a kill
// reported as a crash.
//
// The ordering is forced here (the stopped record is written first, then the
// process finishes), so the test is deterministic rather than a race to
// observe).
func TestWaiterDoesNotOverwriteADeliberateStop(t *testing.T) {
	f := newFixture(t, allowAll(t))
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	res, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	waitInstance(t, f.configDir, res.InstanceID, runtime.StateRunning)

	// What StopService writes when someone cancels.
	path := runtime.InstancePath(f.configDir, res.InstanceID)
	rec, err := runtime.LoadInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	rec.State = runtime.StateStopped
	rec.EndedAt = time.Now().UTC()
	rec.Duration = "1s"
	if err := runtime.SaveInstance(path, rec); err != nil {
		t.Fatal(err)
	}

	// Now let the process finish: the waiter wakes up and must leave the stop
	// alone (the run reports success, so without the guard it would say so).
	f.releaseAndSettle(t)
	time.Sleep(30 * time.Millisecond)

	final, err := runtime.LoadInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != runtime.StateStopped {
		t.Fatalf("the waiter overwrote the stop: state=%s error=%q", final.State, final.Error)
	}
}

// ── run flow ─────────────────────────────────────────────────────────────

func TestRunFlowChecksEveryNodeTool(t *testing.T) {
	f := newFixture(t, allowAll(t))
	samples := "word\nhello\n"

	// The flow uses demo and other; a credential narrowed to one of them must
	// not be able to launder the second through composition.
	narrowed := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, []string{"demo"})
	if _, err := f.RunFlow(narrowed, FlowRunRequest{Flow: "pipe", Samples: samples}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("narrowed run_flow = %v, want ErrForbidden", err)
	}

	open := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	// A flow runs its nodes in order, so a release-the-handles fake would starve
	// the second node; auto-finish keeps the whole run moving.
	f.backend.auto.Store(true)
	res, err := f.RunFlow(open, FlowRunRequest{Flow: "pipe", Samples: samples})
	if err != nil {
		t.Fatal(err)
	}
	if res.RunID == "" || res.Jobs != 2 || res.Samples != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.ToolsUsed) != 2 {
		t.Errorf("toolsUsed = %v, want both node tools", res.ToolsUsed)
	}
	// The run record exists immediately, so a caller can watch it.
	rec, err := flow.LoadRun(flow.RecordPath(config.DataDir(f.configDir), "alice", res.RunID))
	if err != nil {
		t.Fatalf("the run record was not written: %v", err)
	}
	if rec.FlowID != "pipe" {
		t.Errorf("record = %+v", rec)
	}
	// Let the background run finish so it does not outlive the test's fixtures.
	waitRun(t, f.configDir, "alice", res.RunID)
	f.releaseAndSettle(t)
}

// releaseAndSettle lets the fake units finish and waits for the work to be
// over. Without it a background waiter could still be writing into the test's
// temporary directories when they are removed.
func (f *fixture) releaseAndSettle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.backend.finishAll()
		if f.backend.live() == 0 && f.instancesSettled() {
			// One more beat: a run whose prepare was still in flight may only
			// now reach the backend, and it self-finishes because auto is on.
			time.Sleep(10 * time.Millisecond)
			if f.backend.live() == 0 && f.instancesSettled() {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("instances did not settle after the fake backend released them")
}

func (f *fixture) instancesSettled() bool {
	insts, err := runtime.ListInstances(f.configDir)
	if err != nil {
		return false
	}
	for _, i := range insts {
		if !i.State.Terminal() {
			return false
		}
	}
	return true
}

// waitStarted waits until the fake backend has seen n units start, so a test
// never releases before there is anything to release.
func (f *fixture) waitStarted(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.backend.count() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the backend saw %d starts in 2s, want %d", f.backend.count(), n)
}

// waitRun polls a flow run record until it reaches a terminal state.
func waitRun(t *testing.T, configDir, user, runID string) {
	t.Helper()
	path := flow.RecordPath(config.DataDir(configDir), user, runID)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec, err := flow.LoadRun(path); err == nil {
			switch rec.State {
			case flow.RunSucceeded, flow.RunFailed, flow.RunCancelled:
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("flow run %s did not settle", runID)
}

func TestRunFlowNeedsSamples(t *testing.T) {
	f := newFixture(t, allowAll(t))
	open := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	if _, err := f.RunFlow(open, FlowRunRequest{Flow: "pipe"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("no samples = %v, want ErrBadRequest", err)
	}
	if _, err := f.RunFlow(open, FlowRunRequest{Flow: "nope", Samples: "word\nx\n"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown flow = %v, want ErrNotFound", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

// waitInstance polls until the instance record reaches want.
func waitInstance(t *testing.T, configDir, id string, want runtime.State) *runtime.Instance {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last *runtime.Instance
	for time.Now().Before(deadline) {
		rec, err := runtime.LoadInstance(runtime.InstancePath(configDir, id))
		if err == nil {
			last = rec
			if rec.State == want {
				return rec
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("instance %s never reached %s (last: %+v)", id, want, last)
	return nil
}

// The write path is what an auditor asks about: who, which tool version, what
// parameters, and whether it was allowed. This locks both outcomes into the
// structured stream.
func TestSubmitWritesAudit(t *testing.T) {
	f := newFixture(t, allowAll(t))
	rec := audit.New(config.DataDir(f.configDir))
	f.opts.Audit = rec
	defer rec.Close()

	allowed := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	res, err := f.Submit(allowed, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "hi"}})
	if err != nil {
		t.Fatal(err)
	}

	// A read-only token is refused by the submit scope — and the refusal is
	// itself worth recording.
	readOnly := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeRead}, nil)
	if _, err := f.Submit(readOnly, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only submit = %v, want ErrForbidden", err)
	}

	events, err := audit.Query(config.DataDir(f.configDir), audit.Filter{Action: "submit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2: %+v", len(events), events)
	}
	allow := events[0]
	if allow.Decision != audit.Allow || allow.Actor.User != "alice" || allow.Actor.Kind != audit.KindAgentToken {
		t.Fatalf("allow event wrong: %+v", allow)
	}
	if allow.Target.ID != "demo" || allow.Target.Version != "0.1.0" {
		t.Fatalf("allow event lost tool@version: %+v", allow.Target)
	}
	if allow.Params["word"] != "hi" {
		t.Fatalf("allow event lost params: %+v", allow.Params)
	}
	if allow.Refs["job"] != res.JobID || allow.Refs["instance"] != res.InstanceID {
		t.Fatalf("allow event lost refs: %+v", allow.Refs)
	}
	deny := events[1]
	if deny.Decision != audit.Deny || deny.Reason == "" {
		t.Fatalf("deny event wrong: %+v", deny)
	}
}

// A client-supplied idempotency key pins a submission: a retry returns the first
// outcome instead of starting a second run (A2).
func TestSubmitIsIdempotentWithAKey(t *testing.T) {
	f := newFixture(t, allowAll(t))
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	first, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}, IdempotencyKey: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}, IdempotencyKey: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if first.JobID != again.JobID || first.InstanceID != again.InstanceID {
		t.Fatalf("a replay must return the first submission: %+v vs %+v", first, again)
	}

	// A different key is a different submission.
	other, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}, IdempotencyKey: "def"})
	if err != nil {
		t.Fatal(err)
	}
	if other.JobID == first.JobID {
		t.Fatal("a new key must create a new job")
	}

	// No key: every call is a new submission, as before.
	a, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.JobID == b.JobID {
		t.Fatal("without a key there is no dedupe")
	}

	// The key is namespaced by tool: the same string on another tool is a
	// different job.
	onOther, err := f.Submit(ident, SubmitRequest{Tool: "other", Params: map[string]any{"word": "x"}, IdempotencyKey: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if onOther.JobID == first.JobID {
		t.Fatal("the key must be scoped per tool")
	}
}

func TestIdempotentReplayIsAudited(t *testing.T) {
	f := newFixture(t, allowAll(t))
	rec := audit.New(config.DataDir(f.configDir))
	f.opts.Audit = rec
	defer rec.Close()
	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)

	for i := 0; i < 2; i++ {
		if _, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}, IdempotencyKey: "k1"}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := audit.Query(config.DataDir(f.configDir), audit.Filter{Action: "submit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("submit events = %d, want 2", len(events))
	}
	if events[0].Params["idempotent_replay"] == "true" {
		t.Fatal("the first submission is not a replay")
	}
	if events[1].Params["idempotent_replay"] != "true" {
		t.Fatalf("the replay must be marked: %+v", events[1].Params)
	}
}

// 审计的 submit 事件要带上工具包的内容摘要：version 说“声称的版本”，
// digest 说“实际跑的字节”。
func TestSubmitAuditCarriesToolDigest(t *testing.T) {
	f := newFixture(t, allowAll(t))
	rec := audit.New(config.DataDir(f.configDir))
	f.opts.Audit = rec
	defer rec.Close()

	ident := tokenIdentity(t, "alice", []agenttoken.Scope{agenttoken.ScopeSubmit}, nil)
	if _, err := f.Submit(ident, SubmitRequest{Tool: "demo", Params: map[string]any{"word": "x"}}); err != nil {
		t.Fatal(err)
	}

	events, err := audit.Query(config.DataDir(f.configDir), audit.Filter{Action: "submit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("submit events = %d", len(events))
	}
	got := events[0].Target.Digest
	if got == "" {
		t.Fatal("the submit event must carry the tool's content digest")
	}
	// It is the digest of the package on disk, not a made-up string.
	tl, err := tool.Find(f.toolsDir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if got != tl.Digest {
		t.Fatalf("audit digest = %s, want the package's %s", got, tl.Digest)
	}
}
