package main

import (
	"flag"
	"strings"
	"testing"
)

// TestParseFlagsLooseInterleaves pins the fix for a bug that made the CLI lie:
// Go's flag package stops at the first non-flag argument, so
// `srcos grant group bio --user alice` silently ignored --user and then
// reported that the group would be empty.
func TestParseFlagsLooseInterleaves(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantUsers []string
		wantPos   []string
		wantDir   string
	}{
		{
			name:      "flags after a positional",
			args:      []string{"bio-team", "--user", "alice", "--user", "bob"},
			wantUsers: []string{"alice", "bob"},
			wantPos:   []string{"bio-team"},
		},
		{
			name:      "flags before a positional",
			args:      []string{"--user", "alice", "bio-team"},
			wantUsers: []string{"alice"},
			wantPos:   []string{"bio-team"},
		},
		{
			name:      "flags on both sides",
			args:      []string{"--user", "alice", "bio-team", "--user", "bob"},
			wantUsers: []string{"alice", "bob"},
			wantPos:   []string{"bio-team"},
		},
		{
			name:      "two positionals with flags between",
			args:      []string{"one", "--user", "alice", "two"},
			wantUsers: []string{"alice"},
			wantPos:   []string{"one", "two"},
		},
		{
			name:    "shorthand config dir after a positional",
			args:    []string{"bio-team", "-d", "/srv/srcos/config"},
			wantPos: []string{"bio-team"},
			wantDir: "/srv/srcos/config",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			users := &strList{}
			fs.Var(users, "user", "")
			dir := fs.String("d", "", "")
			var positional []string
			parseFlagsLoose(fs, tc.args, &positional)

			if got := strings.Join(*users, ","); got != strings.Join(tc.wantUsers, ",") {
				t.Errorf("users = %q, want %q", got, tc.wantUsers)
			}
			if got := strings.Join(positional, ","); got != strings.Join(tc.wantPos, ",") {
				t.Errorf("positional = %q, want %q", got, tc.wantPos)
			}
			if tc.wantDir != "" && *dir != tc.wantDir {
				t.Errorf("config dir = %q, want %q", *dir, tc.wantDir)
			}
		})
	}
}

func TestStrListAndKVList(t *testing.T) {
	var sl strList
	for _, v := range []string{"a", "b"} {
		if err := sl.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if sl.String() != "a,b" {
		t.Fatalf("strList = %q", sl.String())
	}
	if err := sl.Set("  "); err == nil {
		t.Fatal("an empty value must be rejected, not silently added")
	}

	var kl kvList
	if err := kl.Set("k=v"); err != nil {
		t.Fatal(err)
	}
	if len(kl) != 1 || kl[0].key != "k" || kl[0].value != "v" {
		t.Fatalf("kvList = %+v", kl)
	}
	// A value may itself contain '=' (base64, a URL query, a path).
	if err := kl.Set("k=a=b"); err != nil {
		t.Fatal(err)
	}
	if kl[1].value != "a=b" {
		t.Fatalf("kvList lost part of the value: %+v", kl[1])
	}
	for _, bad := range []string{"noequals", "=v"} {
		if err := kl.Set(bad); err == nil {
			t.Errorf("kvList.Set(%q) should fail", bad)
		}
	}
}
