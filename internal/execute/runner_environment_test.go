package execute

import (
	"testing"

	"github.com/seqyuan/srcos/internal/environment"
	"github.com/seqyuan/srcos/internal/runtime"
)

// The gateway's controller must not build a second runner that drops seams the
// supervisor already has. The environment provider was exactly that bug: a task
// submitted through the gateway failed with "no environment provider is
// configured" even though the gateway itself loaded one.
func TestRunnerForReusesTheSupervisorSeams(t *testing.T) {
	environments, err := environment.New([]environment.Environment{{
		ID:   "demo-env",
		Root: t.TempDir(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	supervisor := runtime.NewRunner(runtime.Options{
		ToolsDir:     t.TempDir(),
		Environments: environments,
	})
	c := New(Options{ToolsDir: t.TempDir(), Supervisor: supervisor})

	runner, err := c.runnerFor("alice")
	if err != nil {
		t.Fatalf("runnerFor: %v", err)
	}
	opts := runner.Options()
	if opts.User != "alice" {
		t.Errorf("user = %q, want alice", opts.User)
	}
	if opts.Environments == nil {
		t.Fatal("the runner lost the environment provider")
	}
	if _, ok := opts.Environments.Get("demo-env"); !ok {
		t.Errorf("environments = %v, want demo-env", opts.Environments.List())
	}
}
