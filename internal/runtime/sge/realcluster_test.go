package sge

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seqyuan/srcos/internal/runtime"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file drives the backend against a real SGE cluster. It is skipped unless
// the environment says so, because it needs a submit host, a shared filesystem
// and a scheduler; none of that belongs in the unit-test loop.
//
// Required environment:
//
//	SRCOS_SGE_REAL=1
//	SRCOS_SGE_SUBMIT_DIR   shared dir for job scripts
//	SRCOS_SGE_RD_DIR       shared dir for the rendezvous control channel
//	SRCOS_SGE_WORK_DIR     shared dir for outputs/logs
//
// Optional:
//
//	SRCOS_SGE_QSUB/QDEL/QSTAT  binary (or wrapper) paths
//	SRCOS_SGE_PE               parallel environment name (default smp)
//
// A node that has the SGE client but is not in submit_hosts can still be
// tested by pointing QSUB/QDEL at wrappers that forward to a submit host; the
// test only needs the scheduler to accept the submission.
func realClusterConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("SRCOS_SGE_REAL") == "" {
		t.Skip("set SRCOS_SGE_REAL=1 (with SRCOS_SGE_* paths) to run against a real cluster")
	}
	need := func(k string) string {
		v := os.Getenv(k)
		if v == "" {
			t.Fatalf("%s must be set for the real-cluster test", k)
		}
		return v
	}
	pe := os.Getenv("SRCOS_SGE_PE")
	if pe == "" {
		pe = "smp"
	}
	return Config{
		Qsub:          os.Getenv("SRCOS_SGE_QSUB"),
		Qstat:         os.Getenv("SRCOS_SGE_QSTAT"),
		Qdel:          os.Getenv("SRCOS_SGE_QDEL"),
		Qalter:        os.Getenv("SRCOS_SGE_QALTER"),
		SubmitDir:     need("SRCOS_SGE_SUBMIT_DIR"),
		RendezvousDir: need("SRCOS_SGE_RD_DIR"),
		Scheduler:     SchedulerDefaults{PE: pe, PEAccounting: "cores"},
		PollEvery:     2 * time.Second,
		Runner:        ExecRunner{},
	}
}

func realTaskRequest(t *testing.T, cfg Config, runID, script string, outDir string) runtime.StartRequest {
	t.Helper()
	tl := &tool.Tool{
		ID:        "probe",
		Kind:      tool.KindTask,
		Backend:   tool.BackendSGE,
		Sandbox:   tool.SandboxNone,
		Resources: tool.Resources{CPU: 1, Memory: "256Mi", Walltime: "0:05:00"},
	}
	return runtime.StartRequest{
		Tool:       tl,
		Argv:       []string{"/bin/bash", "-c", script},
		Env:        []string{"PATH=/usr/bin:/bin", "HOME=" + outDir},
		UnitName:   "srcos-probe-" + runID,
		LogPath:    filepath.Join(outDir, "job.log"),
		InstanceID: "probe-" + runID,
	}
}

func TestRealClusterTaskRunsAndReportsExitZero(t *testing.T) {
	cfg := realClusterConfig(t)
	outDir := filepath.Join(os.Getenv("SRCOS_SGE_WORK_DIR"), "ok-"+time.Now().Format("150405.000"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\n" +
		"hostname > " + shellQuote(outDir+"/host.txt") + "\n" +
		"echo e2e-ok > " + shellQuote(outDir+"/result.txt") + "\n"

	b := &Backend{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	h, err := b.Start(ctx, realTaskRequest(t, cfg, time.Now().Format("150405.000000"), script, outDir))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("submitted job %s", h.Ref())

	// While it exists, the scheduler must confirm it.
	if !b.UnitAlive(ctx, &runtime.Instance{BackendRef: h.Ref()}) {
		t.Errorf("UnitAlive said false for a just-submitted job %s", h.Ref())
	}

	st := h.Wait(ctx)
	if st.Code != 0 || st.Err != nil {
		t.Fatalf("Wait = %+v (log: %s)", st, readIfExists(filepath.Join(outDir, "job.log")))
	}
	if got := readIfExists(filepath.Join(outDir, "result.txt")); strings.TrimSpace(got) != "e2e-ok" {
		t.Errorf("result.txt = %q", got)
	}
	if host := strings.TrimSpace(readIfExists(filepath.Join(outDir, "host.txt"))); host != "" {
		t.Logf("job ran on %s", host)
	}
}

func TestRealClusterTaskReportsNonZeroExit(t *testing.T) {
	cfg := realClusterConfig(t)
	outDir := filepath.Join(os.Getenv("SRCOS_SGE_WORK_DIR"), "fail-"+time.Now().Format("150405.000"))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	h, err := b.Start(ctx, realTaskRequest(t, cfg, time.Now().Format("150405.000000"), "exit 7\n", outDir))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := h.Wait(ctx)
	if st.Code != 7 {
		t.Fatalf("Wait = %+v, want code 7", st)
	}
}

func readIfExists(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// ephemeralPorts is a minimal PortAllocator for the real-cluster test. It asks
// the OS for a free loopback port and hands it over (the tunnel re-binds it).
type ephemeralPorts struct{}

func (ephemeralPorts) Acquire(string) (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
func (ephemeralPorts) Release(int) {}

// TestRealClusterServiceTunnel exercises the whole service path on a real
// cluster: the job picks a port on the compute node, publishes it through the
// rendezvous, and the login node reaches it through `ssh -L`.
func TestRealClusterServiceTunnel(t *testing.T) {
	cfg := realClusterConfig(t)
	cfg.Tunnel = true
	cfg.Ports = ephemeralPorts{}

	runID := time.Now().Format("150405.000000")
	outDir := filepath.Join(os.Getenv("SRCOS_SGE_WORK_DIR"), "svc-"+runID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tl := &tool.Tool{
		ID:        "probe-svc",
		Kind:      tool.KindService,
		Backend:   tool.BackendSGE,
		Sandbox:   tool.SandboxNone,
		Resources: tool.Resources{CPU: 1, Memory: "256Mi", Walltime: "0:05:00"},
		Ingress:   &tool.Ingress{Port: 0},
	}
	req := runtime.StartRequest{
		Tool:         tl,
		Argv:         []string{"/bin/bash", "-c", `python3 -m http.server "$SRCOS_PORT" --bind 127.0.0.1`},
		Env:          []string{"PATH=" + os.Getenv("SRCOS_SGE_TOOL_PATH"), "HOME=" + outDir},
		UnitName:     "srcos-svc-probe-" + runID,
		LogPath:      filepath.Join(outDir, "job.log"),
		InstanceID:   "svc-probe-" + runID,
		WantEndpoint: true,
	}
	b := &Backend{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	h, err := b.Start(ctx, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("submitted service job %s", h.Ref())
	defer func() { _ = h.Stop(context.Background()) }()

	ep, ok := h.Endpoint()
	if !ok {
		t.Fatalf("no endpoint: %v", h.(*jobHandle).endpointErr)
	}
	if ep.Host != "127.0.0.1" {
		t.Fatalf("endpoint is not the tunnel's loopback port: %+v", ep)
	}
	t.Logf("tunnel endpoint %s (log: %s)", ep.String(), readIfExists(filepath.Join(outDir, "job.log")))

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://" + ep.String() + "/")
	if err != nil {
		t.Fatalf("GET through the tunnel failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == 0 {
		t.Fatal("no HTTP status through the tunnel")
	}
	t.Logf("GET through tunnel -> HTTP %d", resp.StatusCode)

	// A gateway restart hands the record back to Reconcile: the ssh child
	// outlives the process, so the surviving forward must be adopted rather than
	// torn down or re-routed to a dead port.
	rec := &runtime.Instance{ID: req.InstanceID, BackendRef: h.Ref(), Endpoint: ep.String()}
	if err := b.ReattachUnit(ctx, rec); err != nil {
		t.Fatalf("a live tunnel must be adopted on reattach: %v", err)
	}
}

// TestRealClusterWalltimeRenewal exercises `qalter -l h_rt=...` against a real
// scheduler: a job near its declared walltime must get its ceiling pushed out.
func TestRealClusterWalltimeRenewal(t *testing.T) {
	cfg := realClusterConfig(t)
	cfg.Scheduler.RenewBefore = 3 * time.Minute
	cfg.Scheduler.RenewFor = 10 * time.Minute

	runID := time.Now().Format("150405.000000")
	outDir := filepath.Join(os.Getenv("SRCOS_SGE_WORK_DIR"), "renew-"+runID)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tl := &tool.Tool{
		ID:        "probe-renew",
		Kind:      tool.KindTask,
		Backend:   tool.BackendSGE,
		Sandbox:   tool.SandboxNone,
		Resources: tool.Resources{CPU: 1, Memory: "256Mi", Walltime: "0:02:00"},
	}
	req := runtime.StartRequest{
		Tool:       tl,
		Argv:       []string{"/bin/bash", "-c", "sleep 40"},
		Env:        []string{"PATH=/usr/bin:/bin", "HOME=" + outDir},
		UnitName:   "srcos-renew-" + runID,
		LogPath:    filepath.Join(outDir, "job.log"),
		InstanceID: "renew-" + runID,
	}
	b := &Backend{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	h, err := b.Start(ctx, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = h.Stop(context.Background()) }()
	t.Logf("submitted job %s", h.Ref())

	// run.go seeds the lease from the declared walltime; this test drives the
	// backend directly, so it seeds the same value.
	inst := &runtime.Instance{
		ID:             req.InstanceID,
		BackendRef:     h.Ref(),
		StartedAt:      time.Now(),
		LeaseExpiresAt: time.Now().Add(2 * time.Minute),
	}
	did, err := b.UnitRenew(ctx, inst, tl)
	if err != nil {
		t.Fatalf("UnitRenew: %v", err)
	}
	if !did {
		t.Fatal("renewal did not fire for a job inside renew_before")
	}
	if !inst.LeaseExpiresAt.After(time.Now().Add(5 * time.Minute)) {
		t.Fatalf("lease not pushed out: %v", inst.LeaseExpiresAt)
	}
	t.Logf("renewed %s until %s", h.Ref(), inst.LeaseExpiresAt.Format(time.RFC3339))
}
