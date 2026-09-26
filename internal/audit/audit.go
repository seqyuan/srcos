// Package audit is the structured, append-only record of who did what.
//
// It exists because "可审计" is one of SRCOS's three pillars (AGENTS.md) and a
// scattered log line is not an audit trail. The design is deliberately plain:
// one JSON object per line, appended to data/audit/audit-YYYY-MM-DD.jsonl, with
// no in-process queue and no database — 文件系统即数据库, same as everything
// else here.
//
// Two rules the rest of the code relies on:
//
//   - Recording must never break the request it describes. A nil *Recorder is a
//     no-op (so a caller can always call Record), and a write failure is logged
//     and dropped, never returned into the request path.
//   - Secrets never reach the file. Every parameter map goes through Redact.
//
// Scope today is the write path (submit / cancel / run_flow), denied access,
// credential lifecycle and authorization changes. Reading is `srcos audit`.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Actor roles. The kind tells an auditor which credential was presented.
const (
	KindSession    = "session"     // browser session cookie
	KindAgentToken = "agent_token" // Authorization: Bearer
	KindCLI        = "cli"         // a local operator running srcos
	KindSystem     = "system"      // a background decision (reconcile, reaper)
	KindAnonymous  = "anonymous"   // refused before an identity was established
)

// Decisions.
const (
	Allow = "allow"
	Deny  = "deny"
)

// Actor is who acted: the user, and which credential spoke for them.
type Actor struct {
	User string `json:"user,omitempty"`
	Kind string `json:"kind,omitempty"`
	// TokenID/Label identify the agent token when Kind is agent_token. The
	// plaintext is never stored anywhere, so an auditor matches an id.
	TokenID string   `json:"token_id,omitempty"`
	Label   string   `json:"label,omitempty"`
	// Instance, when set, names the hosted service instance a minted token
	// belongs to (A1): it is how the audit distinguishes "an instance did this"
	// from "a person did this".
	Instance string   `json:"instance,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
}

// Target is what was acted upon. Version is carried for tools because "哪个版本
// 的工具" is one of the facts the record must answer.
type Target struct {
	Type    string `json:"type,omitempty"` // tool | job | instance | flow | run | token | grant | group
	ID      string `json:"id,omitempty"`
	Version string `json:"version,omitempty"`
	// Digest is the content hash of the tool package when the target is a tool:
	// `version` says which release someone claims, the digest says which bytes
	// actually ran. It is not a signature — see tool.Digest.
	Digest string `json:"digest,omitempty"`
}

// WithDigest attaches the target's content digest (which build of the tool ran).
func (e Event) WithDigest(d string) Event {
	e.Target.Digest = d
	return e
}

// Request is where the act came from, when it came over HTTP. It is what turns
// "someone submitted" into "this client submitted".
type Request struct {
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	IP     string `json:"ip,omitempty"`
	UA     string `json:"ua,omitempty"`
}

// Outcome is how it ended. A denied request carries the reason instead.
type Outcome struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Event is one line of the audit stream.
type Event struct {
	TS       time.Time         `json:"ts"`
	Actor    Actor             `json:"actor"`
	Action   string            `json:"action"`
	Target   Target            `json:"target,omitempty"`
	Params   map[string]string `json:"params,omitempty"`
	Decision string            `json:"decision"`
	Reason   string            `json:"reason,omitempty"`
	Request  *Request          `json:"request,omitempty"`
	Outcome  *Outcome          `json:"outcome,omitempty"`
	// Refs names the things an investigator follows: job / instance / run.
	Refs map[string]string `json:"refs,omitempty"`

	// Prev is the previous line's Hash in the same day-file (empty at the start
	// of a file), and Hash is chainHash over this event with Hash cleared.
	//
	// The chain is per-file on purpose: retention deletes whole day-files, so a
	// chain that crossed files would be broken by ordinary pruning. What it
	// proves is that *this file* was not edited or re-ordered after the fact —
	// not that a whole file was never removed. See Verify.
	Prev string `json:"prev,omitempty"`
	Hash string `json:"hash,omitempty"`
}

// chainHash is the tamper-evidence link: SHA-256 over the previous line's hash
// and this event's own bytes. Marshaling is deterministic (struct field order),
// so the verifier recomputes it the same way.
func chainHash(e Event) string {
	e.Hash = ""
	data, err := json.Marshal(e)
	if err != nil {
		// Cannot happen for this struct; refuse to produce a chain link that
		// would not verify rather than emit a wrong one.
		return ""
	}
	sum := sha256.Sum256(append([]byte(e.Prev+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

// NewEvent starts an event that is allowed unless it is marked Deny.
func NewEvent(actor Actor, action string) Event {
	return Event{Actor: actor, Action: action, Decision: Allow}
}

// Allowed returns e with an OK outcome.
func (e Event) Allowed() Event {
	e.Decision = Allow
	e.Outcome = &Outcome{OK: true}
	return e
}

// Denied returns e carrying the reason, and no OK outcome.
func (e Event) Denied(reason string) Event {
	e.Decision = Deny
	e.Reason = reason
	return e
}

// WithTarget names what was acted on.
func (e Event) WithTarget(kind, id, version string) Event {
	e.Target = Target{Type: kind, ID: id, Version: version}
	return e
}

// WithParams attaches redacted parameters, and does nothing for an empty map.
func (e Event) WithParams(params map[string]any) Event {
	e.Params = Redact(params)
	return e
}

// WithRefs attaches the followable ids (job, instance, run, ...).
func (e Event) WithRefs(refs map[string]string) Event {
	if len(refs) == 0 {
		return e
	}
	if e.Refs == nil {
		e.Refs = map[string]string{}
	}
	for k, v := range refs {
		if v != "" {
			e.Refs[k] = v
		}
	}
	return e
}

// WithRequest attaches the HTTP origin of the act.
func (e Event) WithRequest(method, path, ip, ua string) Event {
	e.Request = &Request{Method: method, Path: path, IP: ip, UA: ua}
	return e
}

// WithError records a failure outcome (for an allowed act that then failed).
func (e Event) WithError(err error) Event {
	if err != nil {
		e.Outcome = &Outcome{OK: false, Error: err.Error()}
	}
	return e
}

// sensitiveKeys are parameter names whose *values* are never written. The
// comparison is case-insensitive and substring-based on purpose: `db_password`
// and `apiKey` must both be caught by a rule nobody has to maintain.
var sensitiveKeys = []string{"password", "passwd", "secret", "token", "apikey", "api_key", "credential", "private"}

// Redact renders a parameter map for the audit file, masking sensitive values.
// It returns nil for an empty map so the JSON stays small.
func Redact(params map[string]any) map[string]string {
	if len(params) == 0 {
		return nil
	}
	out := make(map[string]string, len(params))
	for k, v := range params {
		if isSensitive(k) {
			out[k] = "***"
			continue
		}
		out[k] = fmt.Sprint(v)
	}
	return out
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// Recorder appends events to data/audit/audit-YYYY-MM-DD.jsonl.
//
// A nil *Recorder is valid and records nothing, so callers can hold one
// unconditionally.
// Recorder appends events to data/audit/audit-YYYY-MM-DD.jsonl.
//
// A nil *Recorder is valid and records nothing, so callers can hold one
// unconditionally.
//
// Each append opens the day-file, takes an exclusive lock, re-reads its tail to
// learn the current chain head, then writes — because more than one process
// appends to the same file (the gateway, and a CLI invocation running
// alongside it). Two writers chaining from their own idea of "the last line"
// would break the chain; the lock makes the chain global to the file rather
// than to the process.
type Recorder struct {
	dir string
	mu  sync.Mutex
	// fwd, when set, spools every event for an external sink (the audit's trust
	// anchor). Nil means "local file only".
	fwd *Forwarder
	// warned suppresses repeat logging when the sink is unwritable.
	warned bool
}

// New returns a recorder writing under dataDir/audit (not config/, which is
// declaration state — the audit is runtime state).
func New(dataDir string) *Recorder {
	return &Recorder{dir: filepath.Join(dataDir, "audit")}
}

// Dir is where this recorder writes.
func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// Record appends one event. It is safe for concurrent use and for multiple
// processes, on a nil receiver, and never returns an error: an audit failure
// must not take down the act it was describing.
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.appendLocked(e); err != nil {
		r.warn("%v", err)
	}
}

// appendLocked writes one event under an exclusive file lock, chaining it to
// whatever the file currently ends with.
func (r *Recorder) appendLocked(e Event) error {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(r.dir, "audit-"+e.TS.Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return err
	}
	defer unlockFile(f)

	last, err := lastHashInFile(f)
	if err != nil {
		return err
	}
	e.Prev = last
	e.Hash = chainHash(e)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	r.warned = false
	_, err = f.Write(line)
	// Spool for the external sink after the local write: an event the local file
	// lost is not worth forwarding, and the spool is local/fast so it does not
	// put the network on the request path.
	if err == nil && r.fwd != nil {
		if serr := r.fwd.Spool(e); serr != nil {
			r.warn("forward: %v", serr)
		}
	}
	return err
}

// ForwardTo attaches an external sink. Nil detaches.
func (r *Recorder) ForwardTo(f *Forwarder) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fwd = f
}

// ForwardStatus describes the external sink for the admin page.
func (r *Recorder) ForwardStatus() ForwardStatus {
	if r == nil {
		return ForwardStatus{}
	}
	r.mu.Lock()
	fwd := r.fwd
	r.mu.Unlock()
	if fwd == nil {
		return ForwardStatus{}
	}
	unsent, dropped := fwd.Stats()
	return ForwardStatus{Configured: true, Sink: fwd.SinkName(), Unsent: unsent, Dropped: dropped}
}

// ForwardStatus is what the admin page shows about the external sink.
type ForwardStatus struct {
	Configured bool
	Sink       string
	Unsent     int
	Dropped    int
}

// lastHashInFile returns the Hash of the last well-formed line in f, reading
// only the tail (audit lines are small). "" means the file has no chain yet.
func lastHashInFile(f *os.File) (string, error) {
	end, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if end == 0 {
		return "", nil
	}
	const tail = 64 * 1024
	start := end - tail
	if start < 0 {
		start = 0
	}
	buf := make([]byte, end-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err == nil && e.Hash != "" {
			return e.Hash, nil
		}
	}
	return "", nil
}

// Close exists for callers that defer it. Writes open and close the file per
// event, so there is nothing to release.
func (r *Recorder) Close() error { return nil }

func (r *Recorder) warn(format string, a ...any) {
	if r.warned {
		return
	}
	r.warned = true
	fmt.Fprintf(os.Stderr, "[srcos] audit: "+format+"\n", a...)
}

// ParseSince turns "7d" / "2h30m" / an RFC3339 timestamp into a cutoff time.
// It is shared by `srcos audit` and the admin endpoint so both read a "since"
// the way an operator writes one. Empty means "no cutoff".
func ParseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(s, "d") {
		if days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h"); err == nil {
			return time.Now().Add(-days * 24), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("not a duration or RFC3339 timestamp: %q", s)
}

// Filter narrows a query. Zero values match everything.
type Filter struct {
	User     string
	Action   string
	Decision string
	Since    time.Time
	Until    time.Time
}

// Matches reports whether e passes f.
func (f Filter) Matches(e Event) bool {
	if f.User != "" && e.Actor.User != f.User {
		return false
	}
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if f.Decision != "" && e.Decision != f.Decision {
		return false
	}
	if !f.Since.IsZero() && e.TS.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.TS.After(f.Until) {
		return false
	}
	return true
}

// Problem is one place where the stream does not verify.
type Problem struct {
	File   string
	Line   int
	Reason string
}

// Verify walks every day-file in order and checks the per-file hash chain,
// returning everything that does not add up.
//
// What it proves and what it does not: it detects an edited line, a re-ordered
// line, a deleted line (the next line's Prev stops matching), and a line whose
// chain fields were stripped. It does **not** prove a whole file was never
// removed (retention removes files legitimately), and it cannot stop someone
// who can rewrite every file after the edit. Tamper-evidence here means "this
// file was not quietly altered in place", not "this machine is trusted".
// Lines written before the chain existed are reported as unchained, once per
// file, rather than silently passed.
func Verify(dataDir string) ([]Problem, error) {
	files, err := listFiles(dataDir)
	if err != nil {
		return nil, err
	}
	var problems []Problem
	for _, path := range files {
		problems = append(problems, verifyFile(path)...)
	}
	return problems, nil
}

func verifyFile(path string) []Problem {
	name := filepath.Base(path)
	f, err := os.Open(path)
	if err != nil {
		return []Problem{{File: name, Reason: err.Error()}}
	}
	defer f.Close()

	var problems []Problem
	prev, unchained := "", 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineno := 0
	for sc.Scan() {
		lineno++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			problems = append(problems, Problem{name, lineno, "malformed JSON: " + err.Error()})
			continue
		}
		if e.Hash == "" {
			unchained++
			continue
		}
		if e.Prev != prev {
			problems = append(problems, Problem{name, lineno,
				"chain broken (prev=" + shortHash(e.Prev) + ", want=" + shortHash(prev) + ")"})
		}
		if want := chainHash(e); e.Hash != want {
			problems = append(problems, Problem{name, lineno, "hash mismatch (line altered)"})
		}
		prev = e.Hash
	}
	if err := sc.Err(); err != nil {
		problems = append(problems, Problem{File: name, Reason: err.Error()})
	}
	if unchained > 0 {
		problems = append(problems, Problem{name, lineno, fmt.Sprintf(
			"%d unchained line(s) — written before tamper-evidence existed", unchained)})
	}
	return problems
}

func shortHash(h string) string {
	if h == "" {
		return "(none)"
	}
	if len(h) > 10 {
		return h[:10]
	}
	return h
}

// Query reads the matching events, oldest first. A missing audit directory is
// not an error: nothing has happened yet.
func Query(dataDir string, f Filter) ([]Event, error) {
	files, err := listFiles(dataDir)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, path := range files {
		if err := scan(path, func(e Event) {
			if f.Matches(e) {
				out = append(out, e)
			}
		}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Tail returns the most recent n matching events, oldest first.
func Tail(dataDir string, f Filter, n int) ([]Event, error) {
	all, err := Query(dataDir, f)
	if err != nil {
		return nil, err
	}
	if n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

// ParseKeep turns "90d" / "2160h" / "0" into a retention duration.
func ParseKeep(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		if days, err := time.ParseDuration(strings.TrimSuffix(s, "d") + "h"); err == nil {
			return days * 24, nil
		}
	}
	return time.ParseDuration(s)
}

// Stale returns the whole day-files older than keep, without removing them.
func Stale(dataDir string, keep time.Duration, now time.Time) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	files, err := listFiles(dataDir)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-keep)
	var stale []string
	for _, path := range files {
		day, ok := dayOf(path)
		if !ok {
			continue
		}
		if day.Before(cutoff) {
			stale = append(stale, path)
		}
	}
	return stale, nil
}

// Prune removes whole day-files older than keep, and returns what it removed.
//
// It is deliberately explicit and never automatic: deleting an audit trail is
// an operator's decision, not a side effect of a tick. A keep of zero or less
// removes nothing, so a misread flag cannot wipe the stream. The caller is
// expected to record the removal itself (see `srcos audit prune`), so the fact
// that records were deleted is also on the record.
func Prune(dataDir string, keep time.Duration, now time.Time) ([]string, error) {
	stale, err := Stale(dataDir, keep, now)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return removed, err
		}
		removed = append(removed, filepath.Base(path))
	}
	return removed, nil
}

// dayOf parses audit-YYYY-MM-DD.jsonl into that day's UTC midnight.
func dayOf(path string) (time.Time, bool) {
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "audit-"), ".jsonl")
	t, err := time.Parse("2006-01-02", name)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// listFiles returns the audit files oldest-first.
func listFiles(dataDir string) ([]string, error) {
	dir := filepath.Join(dataDir, "audit")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files) // audit-YYYY-MM-DD sorts chronologically
	return files, nil
}

// scan calls fn for every well-formed event in path. A corrupt line is skipped
// with a warning rather than aborting the read: losing the rest of a day
// because one line was truncated is worse than the truncation.
func scan(path string, fn func(Event)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Audit lines can carry a params map; lift the 64KB default.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			fmt.Fprintf(os.Stderr, "[srcos] audit: skipping malformed line in %s: %v\n", path, err)
			continue
		}
		fn(e)
	}
	return sc.Err()
}
