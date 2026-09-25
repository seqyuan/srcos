package web

import (
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/audit"
)

func TestAuditPageRendersAndEscapes(t *testing.T) {
	events := []audit.Event{
		{
			TS:       time.Now(),
			Actor:    audit.Actor{User: "alice", Kind: audit.KindAgentToken, TokenID: "abc"},
			Action:   "submit",
			Target:   audit.Target{Type: "tool", ID: "demo", Version: "0.1.0"},
			Params:   map[string]string{"word": "<script>alert(1)</script>"},
			Decision: audit.Allow,
			Refs:     map[string]string{"job": "j1"},
			Outcome:  &audit.Outcome{OK: true},
		},
		{
			TS:       time.Now(),
			Actor:    audit.Actor{User: "bob", Kind: audit.KindAgentToken, TokenID: "xyz"},
			Action:   "submit",
			Target:   audit.Target{Type: "tool", ID: "sleeper"},
			Decision: audit.Deny,
			Reason:   "credential may not submit",
		},
	}

	html := AuditPage("SRCOS", "root", events, audit.Filter{Decision: audit.Deny}, nil)

	for _, want := range []string{
		"审计流",
		"alice", "bob",
		"demo@0.1.0",
		"deny", "allow",
		"credential may not submit",
		`action="/admin/audit"`,
		`name="decision"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("page missing %q", want)
		}
	}

	// The parameter must be escaped, never inlined as markup.
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatal("parameter value was not escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("expected the escaped form of the parameter")
	}
}

func TestAuditPageEmptyState(t *testing.T) {
	html := AuditPage("SRCOS", "root", nil, audit.Filter{}, nil)
	if !strings.Contains(html, "没有匹配的审计记录") {
		t.Fatal("empty state missing")
	}
	if strings.Contains(html, "<table") {
		t.Fatal("empty state should not render a table")
	}
}

func TestAuditPageNewestFirst(t *testing.T) {
	old := audit.Event{TS: time.Now().Add(-time.Hour), Actor: audit.Actor{User: "zzolduser"}, Action: "submit", Decision: audit.Allow}
	neu := audit.Event{TS: time.Now(), Actor: audit.Actor{User: "zznewuser"}, Action: "submit", Decision: audit.Allow}
	html := AuditPage("SRCOS", "root", []audit.Event{old, neu}, audit.Filter{}, nil)
	if strings.Index(html, "zznewuser") > strings.Index(html, "zzolduser") {
		t.Fatal("the page should show the newest event first")
	}
}
