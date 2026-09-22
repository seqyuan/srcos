package sge

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seqyuan/srcos/internal/route"
)

// The rendezvous directory is the control channel between a compute node and
// the login node.
//
// Why a directory instead of a protocol: the compute node cannot reliably reach
// the login node, the login node may not be able to dial the compute node, and
// both can always see the shared filesystem. Writing a file is the one operation
// that needs no agent, no listening socket, no authentication, and no extra
// process — and it survives a SRCOS restart, because the state is on disk rather
// than in a connection.
//
// Every write is atomic (write a temporary, then rename) because the login node
// polls concurrently and would otherwise read a half-written file as truth.
//
// Layout, one directory per instance:
//
//	<rendezvous>/<instance-id>/
//	    jobid       the SGE job id, written by the login node
//	    state       submitted|starting|running|exited|failed|deleted
//	    node        the compute node's hostname
//	    pid         the job script's pid on the compute node
//	    endpoint    host:port, for a service
//	    exit_code   the tool's exit status
const (
	FileJobID    = "jobid"
	FileState    = "state"
	FileNode     = "node"
	FilePID      = "pid"
	FileEndpoint = "endpoint"
	FileExitCode = "exit_code"
)

// State values written by the job script.
const (
	StateSubmitted = "submitted"
	StateStarting  = "starting"
	StateRunning   = "running"
	StateExited    = "exited"
	StateFailed    = "failed"
	StateDeleted   = "deleted"
)

// WriteRendezvous writes one field atomically.
func WriteRendezvous(dir, name, value string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ReadRendezvous reads one field. A missing field is an error, never an empty
// success: for liveness questions "no answer" must not read as "alive".
func ReadRendezvous(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// ClearRendezvous removes every field, so a stale endpoint from a previous run
// of the same instance cannot make a healthcheck pass against a dead process.
func ClearRendezvous(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// ReadEndpoint parses the endpoint field into a route target.
//
// The host is validated as loopback-or-remote-allowed here because this value
// originates on a compute node: a machine that has been compromised, or a job
// that writes garbage, must not be able to point the gateway at an arbitrary
// address. A compute node's own address is legitimate; anything resolvable from
// a shared file is not trusted further than that.
func ReadEndpoint(dir string) (route.Target, error) {
	raw, err := ReadRendezvous(dir, FileEndpoint)
	if err != nil {
		return route.Target{}, err
	}
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil {
		return route.Target{}, fmt.Errorf("endpoint %q is not host:port: %w", raw, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return route.Target{}, fmt.Errorf("endpoint %q has an invalid port", raw)
	}
	if host == "" {
		return route.Target{}, fmt.Errorf("endpoint %q has an empty host", raw)
	}
	return route.Target{Host: host, Port: port}, nil
}

// WaitForState polls until the state field equals one of want, or the deadline
// passes. It returns the state it observed.
func WaitForState(dir string, want []string, timeout time.Duration, poll time.Duration) (string, error) {
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		st, err := ReadRendezvous(dir, FileState)
		if err == nil {
			last = st
			for _, w := range want {
				if st == w {
					return st, nil
				}
			}
			if st == StateFailed || st == StateExited || st == StateDeleted {
				// Terminal and not what we wanted: stop waiting, since no
				// later transition is coming.
				return st, fmt.Errorf("job reached %s while waiting for %v", st, want)
			}
		}
		time.Sleep(poll)
	}
	if last == "" {
		last = "(no state written)"
	}
	return last, fmt.Errorf("timed out after %s waiting for %v (last state: %s)", timeout, want, last)
}

// ─────────────────────────────────────────────────────────────────────────
// ssh 本地转发
// ─────────────────────────────────────────────────────────────────────────

// Tunnel is an `ssh -L` local forward from the login node to a process on a
// compute node.
//
// Direction matters and is the reason this is `-L` rather than `-R`: the
// standard HPC trust direction is login -> compute (that is how a user reaches
// the node running their job), whereas compute -> login SSH is often blocked.
// A local forward therefore needs no cluster configuration change.
//
// The tunnel is what makes "instances listen on loopback only" survive contact
// with a scheduler: the tool binds 127.0.0.1 on the compute node, ssh carries
// that to a loopback port on the login node, and the gateway proxies to it
// exactly as it would to a local service.
type Tunnel struct {
	// LocalPort is the loopback port on the login node. Allocate it from the
	// same pool the local backend uses, so two tunnels cannot collide.
	LocalPort int
	// Node is the compute node's hostname.
	Node string
	// RemoteHost, RemotePort is the target on the compute node. The host is
	// almost always 127.0.0.1, because the tool binds loopback there too.
	RemoteHost string
	RemotePort int
	// User is the SSH login; empty means the current user.
	User string
	// SSHBin overrides the ssh binary.
	SSHBin string
	// ExtraArgs are appended to the ssh command line (site-specific options
	// such as -o ProxyJump or a ControlMaster socket).
	ExtraArgs []string
	// ReadyTimeout bounds how long to wait for the forward to work.
	ReadyTimeout time.Duration

	once sync.Once
	cmd  *exec.Cmd
	err  error
}

func (t *Tunnel) sshBin() string {
	if t.SSHBin != "" {
		return t.SSHBin
	}
	return "ssh"
}

// Args renders the ssh command line.
//
// Note what is *not* here: no GatewayPorts, no remote forwarding, no shell. The
// forward is loopback-to-loopback and the session runs no command, which is the
// smallest surface that can carry the traffic.
func (t *Tunnel) Args() []string {
	dest := t.Node
	if t.User != "" {
		dest = t.User + "@" + t.Node
	}
	args := []string{
		"-N", // no command: this session exists only to carry the forward
		"-o", "BatchMode=yes",
		"-o", "ExitOnForwardFailure=yes", // fail fast instead of silently not forwarding
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("127.0.0.1:%d:%s:%d", t.LocalPort, t.RemoteHost, t.RemotePort),
	}
	args = append(args, t.ExtraArgs...)
	return append(args, dest)
}

// Start launches the tunnel and waits until the forward actually works.
//
// Waiting is not optional: ssh reports success for the session before the
// channel is usable, and publishing a route to a not-yet-forwarding port is
// exactly the race that produces intermittent 502s.
func (t *Tunnel) Start(ctx context.Context) error {
	t.once.Do(func() {
		cmd := exec.Command(t.sshBin(), t.Args()...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		// Its own process group so Stop can take down ssh *and* anything it
		// spawned, without touching the caller's group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.err = fmt.Errorf("ssh -L: %w", err)
			return
		}
		t.cmd = cmd

		timeout := t.ReadyTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if conn, err := net.DialTimeout("tcp",
				net.JoinHostPort("127.0.0.1", strconv.Itoa(t.LocalPort)), time.Second); err == nil {
				conn.Close()
				return
			}
			// If ssh already exited, waiting longer cannot help.
			if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
				t.err = fmt.Errorf("ssh exited before the forward became usable")
				return
			}
			select {
			case <-ctx.Done():
				t.err = ctx.Err()
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		t.err = fmt.Errorf("forward to %s:%d was not usable within %s", t.Node, t.RemotePort, timeout)
	})
	return t.err
}

// Alive reports whether the tunnel is still carrying traffic.
func (t *Tunnel) Alive() bool {
	if t.cmd == nil || t.cmd.Process == nil {
		return false
	}
	if t.cmd.ProcessState != nil && t.cmd.ProcessState.Exited() {
		return false
	}
	conn, err := net.DialTimeout("tcp",
		net.JoinHostPort("127.0.0.1", strconv.Itoa(t.LocalPort)), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Stop tears the tunnel down. Idempotent.
func (t *Tunnel) Stop(ctx context.Context) error {
	if t.cmd == nil || t.cmd.Process == nil {
		return nil
	}
	// Signal the group: ssh may have children (a control master, a helper).
	pgid, err := syscall.Getpgid(t.cmd.Process.Pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = t.cmd.Process.Signal(syscall.SIGTERM)
	}

	done := make(chan struct{})
	go func() { _ = t.cmd.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
	}
	if pgid, err := syscall.Getpgid(t.cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = t.cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("ssh tunnel to %s did not exit", t.Node)
	}
	return nil
}

// PID is exposed for reconciliation and tests.
func (t *Tunnel) PID() int {
	if t.cmd == nil || t.cmd.Process == nil {
		return 0
	}
	return t.cmd.Process.Pid
}
