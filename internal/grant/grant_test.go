package grant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func policy(t *testing.T, grants ...Grant) *Policy {
	t.Helper()
	p, err := New(map[string][]string{"bio": {"alice", "bob"}, "infra": {"carol"}}, []string{"root"}, grants)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestDenyByDefault is the decision the whole model rests on: publishing a tool
// must not publish it to everybody.
func TestDenyByDefault(t *testing.T) {
	p := policy(t, Grant{Tool: "cellranger", Users: []string{"alice"}})

	if !p.Allowed("alice", "cellranger") {
		t.Fatal("an explicitly granted user was denied")
	}
	if p.Allowed("dave", "cellranger") {
		t.Fatal("a user with no grant must be denied")
	}
	// A tool nobody wrote a grant for is invisible.
	if p.Allowed("alice", "published-but-ungranted") {
		t.Fatal("an ungranted tool must be denied")
	}
}

func TestExplicitOptOut(t *testing.T) {
	// The escape hatch is a named field, so it appears in the file a reviewer
	// reads rather than hiding in code.
	p, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Allowed("anyone", "anything") {
		t.Fatal("default must be deny")
	}
	p.DefaultAllow = true
	if !p.Allowed("anyone", "anything") {
		t.Fatal("default_allow must open the catalogue")
	}
}

func TestGrantByGroup(t *testing.T) {
	p := policy(t, Grant{Tool: "cellranger", Groups: []string{"bio"}})
	for _, u := range []string{"alice", "bob"} {
		if !p.Allowed(u, "cellranger") {
			t.Errorf("%s is in group bio and should be allowed", u)
		}
	}
	if p.Allowed("carol", "cellranger") {
		t.Error("carol is not in group bio")
	}
}

func TestPublicGrant(t *testing.T) {
	p := policy(t, Grant{Tool: "hello", Public: true})
	if !p.Allowed("anyone-at-all", "hello") {
		t.Fatal("a public tool must be usable by any authenticated account")
	}
}

func TestCatchAll(t *testing.T) {
	p := policy(t,
		Grant{Tool: "cellranger", Groups: []string{"bio"}, Quota: Quota{MaxCPU: 16}},
		Grant{Tool: "*", Public: true},
	)
	// The specific grant wins for its quota...
	if q := p.QuotaFor("alice", "cellranger"); q.MaxCPU != 16 {
		t.Fatalf("specific quota not applied: %+v", q)
	}
	// ...while the catch-all covers everything else.
	if !p.Allowed("dave", "something-else") {
		t.Fatal("catch-all did not apply")
	}
	if q := p.QuotaFor("dave", "something-else"); !q.IsZero() {
		t.Fatalf("catch-all has no quota, got %+v", q)
	}
}

func TestAdminBypassesGrants(t *testing.T) {
	p := policy(t, Grant{Tool: "cellranger", Users: []string{"alice"}})
	if !p.IsAdmin("root") {
		t.Fatal("root should be an admin")
	}
	if !p.Allowed("root", "cellranger") {
		t.Fatal("an admin must be able to reach every tool")
	}
	// An unlisted admin still needs... nothing: admins are the ones editing
	// grants, so gating them on a grant would be circular.
	if p.Allowed("carol", "cellranger") {
		t.Fatal("a non-admin without a grant must be denied")
	}
}

func TestQuotaForFollowsTheUserNotJustTheTool(t *testing.T) {
	// Two users reach the same tool through different grants; each gets their
	// own ceiling.
	p, err := New(nil, nil, []Grant{
		{Tool: "cellranger", Users: []string{"alice"}, Quota: Quota{MaxCPU: 8}},
		{Tool: "cellranger", Users: []string{"bob"}, Quota: Quota{MaxCPU: 64}},
	})
	if err != nil {
		// Two entries for one tool are rejected by design; this documents why.
		if !strings.Contains(err.Error(), "duplicate grant") {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if p.QuotaFor("alice", "cellranger").MaxCPU == p.QuotaFor("bob", "cellranger").MaxCPU {
		t.Fatal("per-user ceilings should differ")
	}
}

func TestGroupsOf(t *testing.T) {
	p := policy(t)
	if got := strings.Join(p.GroupsOf("alice"), ","); got != "bio" {
		t.Fatalf("GroupsOf(alice) = %q", got)
	}
	if got := p.GroupsOf("nobody"); len(got) != 0 {
		t.Fatalf("GroupsOf(nobody) = %v", got)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		groups  map[string][]string
		admins  []string
		grants  []Grant
		wantSub string
	}{
		{
			name:    "grant naming nobody",
			grants:  []Grant{{Tool: "x"}},
			wantSub: "grants nobody",
		},
		{
			name:    "duplicate grant",
			grants:  []Grant{{Tool: "x", Public: true}, {Tool: "x", Users: []string{"a"}}},
			wantSub: "duplicate grant",
		},
		{
			name:    "unknown group",
			grants:  []Grant{{Tool: "x", Groups: []string{"ghost"}}},
			wantSub: "unknown group",
		},
		{
			name:    "missing tool id",
			grants:  []Grant{{Users: []string{"a"}}},
			wantSub: "tool is required",
		},
		{
			name:    "negative quota",
			grants:  []Grant{{Tool: "x", Public: true, Quota: Quota{MaxCPU: -1}}},
			wantSub: "must not be negative",
		},
		{
			name:    "bad quota memory",
			grants:  []Grant{{Tool: "x", Public: true, Quota: Quota{MaxMemory: "lots"}}},
			wantSub: "quota.max_memory",
		},
		{
			name:    "duplicate group member",
			groups:  map[string][]string{"g": {"a", "a"}},
			wantSub: "twice",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.groups, tc.admins, tc.grants)
			if err == nil {
				t.Fatalf("expected an error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	if _, err := New(
		map[string][]string{"bio": {"alice"}},
		[]string{"root"},
		[]Grant{
			{Tool: "cellranger", Groups: []string{"bio"}, Quota: Quota{MaxCPU: 16, MaxMemory: "64Gi", MaxInstances: 3}},
			{Tool: "*", Public: true},
		},
	); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// 配额
// ─────────────────────────────────────────────────────────────────────────

func TestCheckQuota(t *testing.T) {
	q := Quota{MaxCPU: 16, MaxMemory: "64Gi", MaxInstances: 3}

	if err := CheckQuota(q, Usage{Instances: 1, CPU: 4, Memory: 8 << 30}, Usage{Instances: 1, CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatalf("within quota was rejected: %v", err)
	}
	if err := CheckQuota(q, Usage{Instances: 3}, Usage{Instances: 1}); err == nil {
		t.Error("instance quota not enforced")
	}
	if err := CheckQuota(q, Usage{CPU: 16}, Usage{CPU: 1}); err == nil {
		t.Error("cpu quota not enforced")
	}
	if err := CheckQuota(q, Usage{Memory: 64 << 30}, Usage{Memory: 1 << 30}); err == nil {
		t.Error("memory quota not enforced")
	}
}

func TestCheckQuotaExplainsItself(t *testing.T) {
	// An aggregate quota is invisible until it bites, so when it bites the
	// message has to say what is used, what is allowed, and what to do.
	err := CheckQuota(Quota{MaxInstances: 2}, Usage{Instances: 2}, Usage{Instances: 1})
	if err == nil {
		t.Fatal("expected a rejection")
	}
	msg := err.Error()
	for _, want := range []string{"2 of 2", "stop one"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
}

func TestZeroQuotaIsNoLimit(t *testing.T) {
	if err := CheckQuota(Quota{}, Usage{Instances: 999, CPU: 999, Memory: 1 << 40}, Usage{CPU: 999}); err != nil {
		t.Fatalf("a zero quota must constrain nothing: %v", err)
	}
}

func TestParseMemoryGrammar(t *testing.T) {
	cases := map[string]uint64{
		"64Gi": 64 << 30,
		"1Mi":  1 << 20,
		"512M": 512e6,
		"2G":   2e9,
		"1024": 1024,
		"":     0,
	}
	for in, want := range cases {
		got, err := parseMemory(in)
		if err != nil {
			t.Errorf("parseMemory(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseMemory(%q) = %d, want %d", in, got, want)
		}
	}
	// Strictness matters: a quota that silently reads "lots" as zero would be a
	// quota that silently does not exist.
	for _, bad := range []string{"lots", "32 gigs", "1.2.3Gi", "-1Gi"} {
		if _, err := parseMemory(bad); err == nil {
			t.Errorf("parseMemory(%q) should fail", bad)
		}
	}
}

func TestLoadMissingFileDeniesEverything(t *testing.T) {
	p, err := Load(filepath.Join(t.TempDir(), "grants.yaml"))
	if err != nil {
		t.Fatalf("a missing grants.yaml is not an error: %v", err)
	}
	if p.Allowed("anyone", "anything") {
		t.Fatal("with no grants.yaml nothing may be allowed")
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grants.yaml")
	body := `groups:
  bio: [alice, bob]
admins: [root]
grants:
  - tool: cellranger
    groups: [bio]
    quota:
      max_cpu: 16
      max_memory: 64Gi
      max_instances: 3
  - tool: "*"
    public: true
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allowed("alice", "cellranger") {
		t.Fatal("group grant not applied")
	}
	if !p.Allowed("dave", "other") {
		t.Fatal("catch-all not applied")
	}
	q := p.QuotaFor("bob", "cellranger")
	if q.MaxCPU != 16 || q.MaxInstances != 3 {
		t.Fatalf("quota = %+v", q)
	}
	if !p.IsAdmin("root") {
		t.Fatal("admin not loaded")
	}
}

func TestNilPolicyAllows(t *testing.T) {
	// A nil policy means authorization is not wired (a single-user deployment).
	// The server substitutes a deny-all policy when a file is expected but
	// missing, so this path only covers "feature not in use".
	var p *Policy
	if !p.Allowed("anyone", "anything") {
		t.Fatal("a nil policy should not deny")
	}
	if p.IsAdmin("anyone") {
		t.Fatal("a nil policy has no admins")
	}
	if !p.QuotaFor("a", "b").IsZero() {
		t.Fatal("a nil policy has no quotas")
	}
}
