package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/agenttoken"
	"github.com/seqyuan/srcos/internal/config"
)

// srcos token ... maintains config/agent-tokens.yaml.
//
// This is the program-credential surface (ADR-019): an agent or MCP client
// cannot hold a browser session cookie, so it carries a bearer token instead.
// The file stores SHA-256 hashes only — the plaintext is printed once, by
// `create`, and is not recoverable afterwards.
func runTokenCmd(args []string) {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "create", "new":
		runTokenCreate(args)
	case "list", "ls":
		runTokenList(args)
	case "revoke", "rm":
		runTokenRevoke(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown token subcommand: %s\n", sub)
		printTokenUsage()
		os.Exit(1)
	}
}

func printTokenUsage() {
	fmt.Fprintln(os.Stderr, "usage: srcos token <create|list|revoke> [options]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  create --user <name>     mint a token (the plaintext is shown once)")
	fmt.Fprintln(os.Stderr, "      --label <text>       what it is for, e.g. annovibe")
	fmt.Fprintln(os.Stderr, "      --scope <scope>      repeatable; only \"read\" can be issued (default read)")
	fmt.Fprintln(os.Stderr, "      --expires <when>     90d | 12h | 2026-12-21 | never (default 90d)")
	fmt.Fprintln(os.Stderr, "  list [--user <name>]     show tokens, expiry and last use")
	fmt.Fprintln(os.Stderr, "  revoke <id>              revoke one token (by id from `list`)")
	fmt.Fprintln(os.Stderr, "  revoke --user <name> --all")
	fmt.Fprintln(os.Stderr, "                           revoke every token of one user")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Store: config/agent-tokens.yaml (SHA-256 hashes only).")
	fmt.Fprintln(os.Stderr, "A token acts as its user and can only narrow that: the Grant policy still")
	fmt.Fprintln(os.Stderr, "decides which tools the user may see or run. Scopes never widen access.")
	fmt.Fprintln(os.Stderr, "The gateway re-reads the file on every check, so create/revoke are immediate.")
}

// tokenStore opens the registry for a config directory.
func tokenStore(configDir string) *agenttoken.Store {
	return agenttoken.New(config.AgentTokensPath(configDir))
}

// revokeTokensFor drops every token of a user, reporting how many were
// removed. Used by `srcos del`: a deleted account must not leave a working
// credential behind, and recreating the same name must not resurrect the old
// token.
func revokeTokensFor(configDir, user string) (int, error) {
	removed, err := tokenStore(configDir).RevokeAllFor(user)
	if err != nil {
		return 0, err
	}
	return len(removed), nil
}

func runTokenCreate(args []string) {
	fs := newFlagSet("token create")
	configDir := configDirFlag(fs)
	user := fs.String("user", "", "owner username (required)")
	label := fs.String("label", "", "what this token is for")
	scopeFlags := &strList{}
	fs.Var(scopeFlags, "scope", "scope (repeatable; default read)")
	expires := fs.String("expires", "90d", "90d | 12h | 2026-12-21 | never")
	var positional []string
	fs.Usage = func() { printTokenUsage() }
	parseFlagsLoose(fs, args, &positional)

	if strings.TrimSpace(*user) == "" {
		fatalf("--user is required: a token belongs to an account")
	}
	// The account must exist, or the token could never authenticate and the
	// operator would blame the gateway instead of the typo.
	if _, err := os.Stat(config.UserConfigPath(*configDir, *user)); err != nil {
		fatalf("user %s does not exist (create it with `srcos user %s`)", *user, *user)
	}

	expiresAt, err := parseTokenExpiry(*expires, time.Now())
	if err != nil {
		fatalf("%v", err)
	}

	scopes := make([]agenttoken.Scope, 0, len(*scopeFlags))
	for _, s := range *scopeFlags {
		scopes = append(scopes, agenttoken.Scope(s))
	}

	store := tokenStore(*configDir)
	rec, plaintext, err := store.Create(agenttoken.CreateParams{
		User:      *user,
		Label:     *label,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		fatalf("%v", err)
	}

	expiry := "never"
	if !rec.ExpiresAt.IsZero() {
		expiry = fmt.Sprintf("%s (%s)", rec.ExpiresAt.Local().Format("2006-01-02 15:04"), humanUntil(rec.ExpiresAt, time.Now()))
	}
	fmt.Printf("created agent token %s for %s\n", rec.ID, rec.User)
	fmt.Printf("  label     %s\n", dash(rec.Label))
	fmt.Printf("  scopes    %s\n", agenttoken.ScopeSet(rec.Scopes).String())
	fmt.Printf("  expires   %s\n", expiry)
	fmt.Printf("  store     %s (SHA-256 hash only)\n", store.Path())
	fmt.Println()
	fmt.Println(plaintext)
	fmt.Println()
	fmt.Println("This is the only time the token is shown — only its hash is stored.")
	fmt.Println("Send it as:")
	fmt.Printf("  Authorization: Bearer %s\n", plaintext)
}

func runTokenList(args []string) {
	fs := newFlagSet("token list")
	configDir := configDirFlag(fs)
	user := fs.String("user", "", "only this user's tokens")
	var positional []string
	fs.Usage = func() { printTokenUsage() }
	parseFlagsLoose(fs, args, &positional)

	store := tokenStore(*configDir)
	if err := store.Reload(); err != nil {
		fatalf("%v", err)
	}
	tokens := store.Tokens()
	if *user != "" {
		kept := tokens[:0]
		for _, t := range tokens {
			if t.User == *user {
				kept = append(kept, t)
			}
		}
		tokens = kept
	}
	if len(tokens) == 0 {
		if *user != "" {
			fmt.Printf("no agent tokens for %s (%s)\n", *user, store.Path())
		} else {
			fmt.Printf("no agent tokens (%s)\n", store.Path())
		}
		fmt.Println("mint one with: srcos token create --user <name> --label <text>")
		return
	}

	// Newest first: the token an operator is looking for is usually the one
	// they just created.
	usage := agenttoken.LoadUsage(config.AgentTokenUsagePath(*configDir))
	now := time.Now()
	fmt.Printf("%-8s %-10s %-16s %-8s %-16s %-16s %-16s %s\n",
		"ID", "USER", "LABEL", "SCOPES", "CREATED", "EXPIRES", "LAST USED", "STATUS")
	for i := len(tokens) - 1; i >= 0; i-- {
		t := tokens[i]
		last := "-"
		if at := usage.Last(t.ID); !at.IsZero() {
			last = at.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("%-8s %-10s %-16s %-8s %-16s %-16s %-16s %s\n",
			t.ID, t.User, truncate(dash(t.Label), 16), agenttoken.ScopeSet(t.Scopes).String(),
			t.CreatedAt.Local().Format("2006-01-02 15:04"),
			expiryCell(t), last, t.Status(now))
	}
	fmt.Printf("\n%d token(s) from %s\n", len(tokens), store.Path())
	fmt.Println("revoke with: srcos token revoke <id>")
}

func expiryCell(t agenttoken.Token) string {
	if t.ExpiresAt.IsZero() {
		return "never"
	}
	return t.ExpiresAt.Local().Format("2006-01-02 15:04")
}

func runTokenRevoke(args []string) {
	fs := newFlagSet("token revoke")
	configDir := configDirFlag(fs)
	user := fs.String("user", "", "revoke every token of this user")
	all := fs.Bool("all", false, "confirm a bulk revoke")
	var positional []string
	fs.Usage = func() { printTokenUsage() }
	parseFlagsLoose(fs, args, &positional)

	store := tokenStore(*configDir)

	if *user != "" {
		if !*all {
			fatalf("refusing to revoke every token of %s without --all", *user)
		}
		removed, err := store.RevokeAllFor(*user)
		if err != nil {
			fatalf("%v", err)
		}
		if len(removed) == 0 {
			fmt.Printf("no agent tokens for %s\n", *user)
			return
		}
		for _, t := range removed {
			fmt.Printf("revoked %s (%s)\n", t.ID, dash(t.Label))
		}
		fmt.Printf("%d token(s) revoked for %s; the gateway picks this up on its next request\n", len(removed), *user)
		return
	}

	if len(positional) == 0 {
		printTokenUsage()
		os.Exit(1)
	}
	id := positional[0]
	rec, found, err := store.Revoke(id)
	if err != nil {
		fatalf("%v", err)
	}
	if !found {
		fatalf("no agent token with id %q (see `srcos token list`)", id)
	}
	fmt.Printf("revoked %s (%s, %s)\n", rec.ID, rec.User, dash(rec.Label))
	fmt.Println("the gateway picks this up on its next request; no restart needed")
}

// parseTokenExpiry accepts "never", "<n>d" / "<n>h", a bare number of days, or
// a date / RFC3339 timestamp. A bare number means days because that is how
// people say it out loud ("expires in 90").
func parseTokenExpiry(s string, now time.Time) (time.Time, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", "never", "none", "0":
		return time.Time{}, nil
	}

	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		if err != nil {
			return time.Time{}, fmt.Errorf("cannot parse expiry %q (want e.g. 90d or never)", s)
		}
		return now.AddDate(0, 0, n), nil
	}
	if strings.HasSuffix(v, "h") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "h"))
		if err != nil {
			return time.Time{}, fmt.Errorf("cannot parse expiry %q (want e.g. 12h or never)", s)
		}
		return now.Add(time.Duration(n) * time.Hour), nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return now.AddDate(0, 0, n), nil
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02 15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse expiry %q (want 90d, 12h, 2026-12-21, or never)", s)
}

// humanUntil renders a coarse "in 89 days" for the creation output.
func humanUntil(t, now time.Time) string {
	d := t.Sub(now)
	switch {
	case d <= 0:
		return "expired"
	case d < 48*time.Hour:
		return fmt.Sprintf("in %d hours", int(d.Hours()+0.5))
	default:
		return fmt.Sprintf("in %d days", int(d.Hours()/24+0.5))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
