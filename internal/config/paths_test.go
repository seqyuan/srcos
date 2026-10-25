package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A hosted agent's credential must live outside the virtual home. Every
// instance of one user mounts that home, so a 0600 file in it is readable by
// the user's other instances (they share the same OS uid).
func TestInstanceTokenPathIsOutsideTheVirtualHome(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "config")
	tokenPath := InstanceTokenPath(configDir, "alice-web-svc")
	home := HomeDir(configDir, "alice")

	rel, err := filepath.Rel(home, tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
		t.Fatalf("instance token %s is inside the shared home %s", tokenPath, home)
	}
	if !strings.Contains(tokenPath, "instance-credentials") {
		t.Fatalf("token path = %s, want the dedicated credential root", tokenPath)
	}
}
