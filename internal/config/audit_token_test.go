package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAuditStateResolveToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("  secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got, err := (&AuditState{ForwardToken: "inline", ForwardTokenFile: path}).ResolveToken(); err != nil || got != "inline" {
		t.Fatalf("inline token must win: got %q err %v", got, err)
	}
	if got, err := (&AuditState{ForwardTokenFile: path}).ResolveToken(); err != nil || got != "secret-value" {
		t.Fatalf("file token must be read and trimmed: got %q err %v", got, err)
	}
	if got, err := (&AuditState{}).ResolveToken(); err != nil || got != "" {
		t.Fatalf("no token configured must be an empty token, not an error: got %q err %v", got, err)
	}
	if _, err := (&AuditState{ForwardTokenFile: filepath.Join(dir, "missing")}).ResolveToken(); err == nil {
		t.Fatal("an unreadable token file must be an error, not an empty token")
	}
}
