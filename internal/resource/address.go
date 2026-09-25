// Package resource implements SRCOS's resource address grammar and the viewer
// registry that decides what to do with an address.
//
// It is deliberately the shallow half: parsing and claiming are pure functions
// with no filesystem and no HTTP, so the rules can be tested on their own and
// reused by every front-end (the viewer page and the REST API). Resolution —
// turning an address into a host path — lives in package inspect, behind the one
// jail (AGENTS.md: Jail is the only path resolution entry point).
//
// The address syntax is intentionally isomorphic to dsh's
// `dsh-resource://<protocol>/<scope>/<path>` (ADR-011/016): an adapter in either
// direction degrades to a string rewrite.
//
//		srcos://<provider>/<scope>/<path>[?tool=<tool-id>]
//
//	  - provider names the kind of resource. Only `file` is implemented; the
//	    slot exists so the scheme can grow without a second URL scheme.
//	  - scope is where the address is rooted. `home` and `workspace` are reserved
//	    builtin scopes; any other scope is a storage id (ADR-020). **An address
//	    never contains a username**: it is resolved as the requesting user, so
//	    cross-user addressing is impossible by construction (ADR-021).
//	  - path is percent-encoded and relative to the scope root (`""` is the root).
//	  - tool is required for scope `workspace`, because `/workspace`'s host
//	    directory depends on the tool (ADR-021) — the same disambiguator
//	    `/api/paths` takes, spelled as a query parameter so the path stays a path.
package resource

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Scheme is the address scheme.
const Scheme = "srcos"

// Providers. `file` is the only implementation; the constant exists so callers
// never spell the string twice.
const ProviderFile = "file"

// Reserved scope names. A storage id may not use them (checked at registration
// in package storage), otherwise "home" would be ambiguous.
const (
	ScopeHome      = "home"
	ScopeWorkspace = "workspace"
)

// ErrInvalid is the class for a malformed address. A syntactically valid
// address that names something the user may not reach fails in resolution, not
// here: this package has no idea who is asking.
var ErrInvalid = errors.New("invalid resource address")

// scopeRe is the identifier alphabet shared with tool ids, storage ids and flow
// ids. Keeping one alphabet means a scope can never need escaping.
var scopeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Address is a parsed resource address.
type Address struct {
	Provider string
	Scope    string
	// Path is the location inside the scope, slash-separated, without a leading
	// slash. "" means the scope root.
	Path string
	// Tool narrows scope=workspace ("" for every other scope).
	Tool string
}

// Parse reads an address.
//
// It hand-parses rather than calling url.Parse: a caller passes the address as
// a query parameter, so it has already been URL-decoded once by net/http, and
// letting url.Parse decode a second time would turn a file named `a%20b.txt`
// into `a b.txt`. Here the query is split off first, then the path is decoded
// exactly once.
func Parse(s string) (Address, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Address{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	rest, ok := strings.CutPrefix(raw, Scheme+"://")
	if !ok {
		return Address{}, fmt.Errorf("%w: must start with %s://", ErrInvalid, Scheme)
	}

	// Query first: everything after the first `?` is the query, so a path
	// containing a `?` must percent-encode it.
	query := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		query, rest = rest[i+1:], rest[:i]
	}

	provider, tail := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		provider, tail = rest[:i], rest[i:]
	}
	if provider == "" {
		return Address{}, fmt.Errorf("%w: missing provider (expected %s://%s/...)", ErrInvalid, Scheme, ProviderFile)
	}
	if provider != ProviderFile {
		return Address{}, fmt.Errorf("%w: unknown provider %q (only %q is implemented)", ErrInvalid, provider, ProviderFile)
	}
	if !strings.HasPrefix(tail, "/") {
		return Address{}, fmt.Errorf("%w: missing scope after %s://%s/", ErrInvalid, Scheme, provider)
	}

	body := tail[1:]
	scope, pathPart := body, ""
	if i := strings.IndexByte(body, '/'); i >= 0 {
		scope, pathPart = body[:i], body[i+1:]
	}
	if scope == "" {
		return Address{}, fmt.Errorf("%w: missing scope (expected home, workspace or a storage id)", ErrInvalid)
	}
	if !scopeRe.MatchString(scope) {
		return Address{}, fmt.Errorf("%w: scope %q must match %s", ErrInvalid, scope, scopeRe)
	}

	path, err := url.PathUnescape(pathPart)
	if err != nil {
		return Address{}, fmt.Errorf("%w: path is not valid percent-encoding: %v", ErrInvalid, err)
	}
	if strings.ContainsRune(path, 0) {
		return Address{}, fmt.Errorf("%w: path contains a NUL byte", ErrInvalid)
	}
	// Normalize the trailing slash: it is a presentation hint, not part of the
	// name, and keeping it would make two spellings of one address compare
	// unequal.
	path = strings.TrimPrefix(path, "/")
	path = strings.TrimSuffix(path, "/")

	tool, err := parseQuery(query)
	if err != nil {
		return Address{}, err
	}
	switch {
	case scope == ScopeWorkspace && tool == "":
		return Address{}, fmt.Errorf("%w: scope workspace requires ?tool=<id> (the workspace directory depends on the tool)", ErrInvalid)
	case scope != ScopeWorkspace && tool != "":
		return Address{}, fmt.Errorf("%w: ?tool= only applies to scope %s, not %q", ErrInvalid, ScopeWorkspace, scope)
	}

	return Address{Provider: provider, Scope: scope, Path: path, Tool: tool}, nil
}

// parseQuery accepts only `tool`, and only once. Anything else is refused
// rather than ignored: a typo'd parameter that silently does nothing is worse
// than an error that says so.
func parseQuery(query string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", nil
	}
	values, err := url.ParseQuery(query)
	if err != nil {
		return "", fmt.Errorf("%w: bad query: %v", ErrInvalid, err)
	}
	tool := ""
	for k, vs := range values {
		if k != "tool" {
			return "", fmt.Errorf("%w: unknown parameter %q (only ?tool= is defined)", ErrInvalid, k)
		}
		if len(vs) != 1 {
			return "", fmt.Errorf("%w: ?tool= may appear once", ErrInvalid)
		}
		tool = strings.TrimSpace(vs[0])
	}
	if tool != "" && !scopeRe.MatchString(tool) {
		return "", fmt.Errorf("%w: tool %q must match %s", ErrInvalid, tool, scopeRe)
	}
	return tool, nil
}

// String renders the address back to its canonical form.
func (a Address) String() string {
	var b strings.Builder
	b.WriteString(Scheme)
	b.WriteString("://")
	b.WriteString(a.Provider)
	b.WriteString("/")
	b.WriteString(a.Scope)
	if a.Path != "" {
		b.WriteString("/")
		b.WriteString(escapePath(a.Path))
	}
	if a.Tool != "" {
		b.WriteString("?tool=")
		b.WriteString(url.QueryEscape(a.Tool))
	}
	return b.String()
}

// Parent returns the enclosing directory within the scope, and false at the
// scope root (where there is nothing above to show).
func (a Address) Parent() (Address, bool) {
	if a.Path == "" {
		return Address{}, false
	}
	i := strings.LastIndex(a.Path, "/")
	out := a
	if i < 0 {
		out.Path = ""
		return out, true
	}
	out.Path = a.Path[:i]
	return out, true
}

// escapePath percent-encodes each segment of a slash-separated path (escaping
// the whole path would escape the separators too).
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
