package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
)

// The queue must be wired from the *resolved* tool directory. A deployment
// that relies on the documented default ($SRCOS_TOOLS_DIR or
// <program dir>/srcos-tools) passes Options.ToolsDir == "", and gating the
// queue on the raw option silently disabled "submit and it runs".
func TestTaskQueueUsesTheResolvedDefaultToolsDir(t *testing.T) {
	base := t.TempDir()
	configDir := filepath.Join(base, "config")
	toolsDir := filepath.Join(base, "srcos-tools")
	if err := os.MkdirAll(filepath.Join(toolsDir, "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := &config.StateConfig{
		Server: config.ServerState{Host: "127.0.0.1", Port: 0},
		Auth:   config.AuthState{SessionSecret: "test-secret", SessionTTL: 3600},
	}
	srv := NewWithOptions(state, configDir, Options{})
	if srv.taskQueue == nil {
		t.Fatal("task queue is nil with an empty ToolsDir, but the default tools directory exists")
	}
}
