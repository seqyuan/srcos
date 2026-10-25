package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
	"github.com/seqyuan/srcos/internal/portpool"
	"github.com/seqyuan/srcos/internal/route"
	"github.com/seqyuan/srcos/internal/sandbox"
	"github.com/seqyuan/srcos/internal/tool"
)

type fakeIssuer struct {
	minted, revoked int
}

func (f *fakeIssuer) MintInstance(user, instanceID string, scopes, tools []string) (string, error) {
	f.minted++
	return "srcos_test_secret", nil
}
func (f *fakeIssuer) RevokeInstance(instanceID string) (bool, error) {
	f.revoked++
	return true, nil
}

// captureBackend returns a handle whose endpoint is an already-running test
// server, so StartService can complete its healthcheck without a real process.
type captureBackend struct {
	target route.Target
	req    StartRequest
}

func (b *captureBackend) Name() string { return "capture" }
func (b *captureBackend) Start(ctx context.Context, req StartRequest) (Handle, error) {
	b.req = req
	return &externalHandle{target: b.target}, nil
}

func TestStartServiceMountsTheCredentialPerInstance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	target, err := route.ParseTarget(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tl := &tool.Tool{
		ID: "web", Version: "0.1.0", Name: "Web",
		Kind: tool.KindService, Backend: "local", Sandbox: tool.SandboxBwrap,
		Command: []string{"true"}, Dir: t.TempDir(),
		Resources: tool.Resources{CPU: 1, Memory: "64Mi"},
		Ingress: &tool.Ingress{
			Healthcheck: &tool.Healthcheck{Path: "/", Timeout: "0:00:05"},
		},
		Lifecycle: &tool.Lifecycle{Restart: "never", MaxLifetime: "1h"},
		Agent:     &tool.AgentSpec{MCP: []string{"read"}},
	}
	ports := portpool.New(24500, 24520)
	routes := route.NewTable()
	issuer := &fakeIssuer{}
	backend := &captureBackend{target: target}
	runner := NewRunner(Options{
		ConfigDir: configDir, ToolsDir: t.TempDir(), User: "alice",
		Backends:    map[string]Backend{"local": backend},
		Routes:      routes,
		AgentTokens: issuer,
	})
	runner.SetPorts(ports)

	inst, err := runner.StartService(context.Background(), tl, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runner.StopService(context.Background(), inst) }()
	if inst.State != StateRunning {
		t.Fatalf("state = %s (%s)", inst.State, inst.Error)
	}
	if issuer.minted != 1 {
		t.Fatalf("minted = %d, want 1", issuer.minted)
	}
	if !envHas(backend.req.Env, "SRCOS_AGENT_TOKEN_FILE="+sandbox.PathAgentToken) {
		t.Fatalf("agent token env missing from %v", backend.req.Env)
	}
	tokenPath := config.InstanceTokenPath(configDir, inst.ID)
	host, _, err := backend.req.Spec.Resolve(sandbox.PathAgentToken)
	if err != nil {
		t.Fatalf("credential is not mounted: %v", err)
	}
	if host != tokenPath {
		t.Fatalf("credential mount = %s, want %s", host, tokenPath)
	}
	// The host file must be outside the shared virtual home.
	home := config.HomeDir(configDir, "alice")
	if strings.HasPrefix(tokenPath, home+string(filepath.Separator)) {
		t.Fatalf("credential %s is inside the shared home %s", tokenPath, home)
	}
}
