package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type StateConfig struct {
	Server ServerState `yaml:"server"`
	Auth   AuthState   `yaml:"auth"`
	SSO    SSOState    `yaml:"sso"`
	Audit  AuditState  `yaml:"audit,omitempty"`
	SGE    SGEState    `yaml:"sge,omitempty"`
}

// SGEState configures the qsub/SGE backend: a cluster where the tool runs on a
// compute node while the gateway stays on a login node.
//
// It lives in state.yaml, not in a tool manifest, because it describes the
// *site* rather than the tool — the same 环境与数据 split that keeps cluster
// defaults out of tool.yaml. An absent or Enabled:false section means this host
// has no scheduler, and a `backend: sge` tool is refused with a clear error
// instead of running in the wrong place.
type SGEState struct {
	Enabled bool `yaml:"enabled"`
	// SubmitDir and RendezvousDir must be on a filesystem shared by the login
	// node and the compute nodes: the job script is read from the first, and the
	// control channel (endpoint, state, exit code) is written to the second.
	SubmitDir     string `yaml:"submit_dir"`
	RendezvousDir string `yaml:"rendezvous_dir"`
	// PE is the parallel environment name; cpu is mapped onto `-pe <PE> <n>`.
	PE             string `yaml:"pe"`
	PEAccounting   string `yaml:"pe_accounting,omitempty"` // cores | threads
	ThreadsPerCore int    `yaml:"threads_per_core,omitempty"`
	DefaultQueue   string `yaml:"default_queue,omitempty"`
	Project        string `yaml:"project,omitempty"`
	// Binary paths; empty means "find on PATH".
	Qsub   string `yaml:"qsub,omitempty"`
	Qstat  string `yaml:"qstat,omitempty"`
	Qdel   string `yaml:"qdel,omitempty"`
	Qalter string `yaml:"qalter,omitempty"`
	// SSH is the binary used for the local forward; empty means "ssh".
	SSH string `yaml:"ssh,omitempty"`
	// SSHUser is the login name for `ssh -L`; empty means the current user.
	SSHUser string `yaml:"ssh_user,omitempty"`
	// SSHArgs are extra ssh options (ProxyJump, a ControlMaster socket, ...).
	SSHArgs []string `yaml:"ssh_args,omitempty"`
	// Tunnel enables the ssh -L data channel. It is a *bool because the default
	// is true and "absent" must be distinguishable from "explicitly false".
	Tunnel *bool `yaml:"tunnel,omitempty"`
	// DirectDial publishes the compute node's own address instead of tunnelling.
	// The route layer only accepts loopback targets, so this works only when the
	// gateway itself runs on the compute node; prefer Tunnel.
	DirectDial bool `yaml:"direct_dial,omitempty"`
	// PollSeconds is how often the rendezvous and qstat are consulted.
	PollSeconds int `yaml:"poll_seconds,omitempty"`
	// RenewBefore / RenewFor enable walltime renewal for long-running services
	// (ADR-015): when a job is within RenewBefore of its h_rt, SRCOS extends it
	// by RenewFor with `qalter -l h_rt=...`. Zero RenewBefore disables renewal,
	// and then a service simply ends when its declared walltime does.
	RenewBefore string `yaml:"renew_before,omitempty"`
	RenewFor    string `yaml:"renew_for,omitempty"`
	// WarnBefore emits a warning (audit + log) when a service is within this
	// window of its h_rt. It is the fallback when renewal is off, and the
	// signal when renewal is on but failing. Zero disables it.
	WarnBefore string `yaml:"warn_before,omitempty"`
}

// AuditState configures where the structured audit stream is forwarded.
//
// Forwarding is the audit's *trust anchor*: the per-file hash chain proves a
// file was not edited in place, but whoever can rewrite every file can recompute
// it. A copy on another machine cannot be rewritten from here. SRCOS therefore
// does not offer local signing — an HMAC key on the same host buys almost
// nothing and looks like more than it is.
//
// Empty ForwardURL means "keep the stream local only" (the default).
type AuditState struct {
	// ForwardURL is an HTTP collector; each event is POSTed as JSON.
	ForwardURL string `yaml:"forward_url,omitempty"`
	// ForwardToken, when set, is sent as `Authorization: Bearer`.
	ForwardToken string `yaml:"forward_token,omitempty"`
	// ForwardMax bounds the on-disk spool in events. Past it the oldest are
	// dropped and counted (never silently). Zero uses a 100k default.
	ForwardMax int `yaml:"forward_max,omitempty"`
}

// SSOState configures the lightweight single sign-on layer: the gateway
// authenticates the user once and vouches for their identity to proxied
// backends by setting a request header on every forwarded request.
//
//   - UserHeader: name of the header carrying the logged-in username
//     (e.g. X-Authenticated-User). Empty disables the feature.
//   - HMACSecret: optional shared secret. When set, every forwarded request
//     also carries UserHeader+"-Signature": an HMAC-SHA256 of
//     "srcos-sso:v1:"+username (base64url, unpadded) that backends can
//     verify without relying on network isolation alone.
//
// Security note: the header is the backend's only proof of identity, so the
// backend must be reachable only through the gateway (loopback/firewall).
// A directly reachable backend could be impersonated with a forged header;
// HMACSecret fixes that even when the backend cannot be network-isolated.
type SSOState struct {
	UserHeader string `yaml:"user_header,omitempty"`
	HMACSecret string `yaml:"hmac_secret,omitempty"`
}

// IsValidHeaderName reports whether s is a valid HTTP header field name
// (RFC 7230 token). http.Header.Set panics on invalid names, so the name is
// validated at configuration time and guarded again before every use.
func IsValidHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		if strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

type ServerState struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	TrustedProxy string `yaml:"trusted_proxy,omitempty"` // CIDR of a trusted reverse proxy; enables X-Forwarded-* header trust
	Title        string `yaml:"title,omitempty"`         // site title shown in the top-left corner (default: SRCOS)
	// Native TLS termination. When both are set the gateway serves HTTPS,
	// making the whole site a secure context (all Web Crypto / secure-context
	// APIs become available to proxied apps). Paths are stored as given;
	// --tls-selfsigned writes absolute paths into the config dir.
	TLSCert string `yaml:"tls_cert,omitempty"`
	TLSKey  string `yaml:"tls_key,omitempty"`
}

type AuthState struct {
	SessionSecret string `yaml:"session_secret"`
	SessionTTL    int    `yaml:"session_ttl"`
}

func defaultState() StateConfig {
	return StateConfig{
		Server: ServerState{
			Host: DefaultHost,
			Port: DefaultPort,
		},
		Auth: AuthState{
			SessionSecret: "",
			SessionTTL:    DefaultSessionTTL,
		},
	}
}

func EnsureState(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	s := defaultState()
	return writeStateFile(path, &s)
}

func LoadState(path string) (*StateConfig, error) {
	if err := EnsureState(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}

	var s StateConfig
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}

	// Apply defaults
	if s.Server.Host == "" {
		s.Server.Host = DefaultHost
	}
	if s.Server.Port == 0 {
		s.Server.Port = DefaultPort
	}
	if s.Auth.SessionTTL == 0 {
		s.Auth.SessionTTL = DefaultSessionTTL
	}

	// A hand-edited user_header must be a valid header name; anything else
	// would panic http.Header.Set on every proxied request.
	if s.SSO.UserHeader != "" && !IsValidHeaderName(s.SSO.UserHeader) {
		fmt.Printf("[srcos] WARNING: ignoring invalid sso.user_header %q (must be a valid HTTP header name)\n", s.SSO.UserHeader)
		s.SSO.UserHeader = ""
		s.SSO.HMACSecret = ""
	}

	// Generate session secret if empty
	if s.Auth.SessionSecret == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
		s.Auth.SessionSecret = hex.EncodeToString(secret)

		// Persist it without dropping any section written by another command
		// (audit, sge, ...): state.yaml is hand-edited too, so a rewrite must
		// preserve what it does not itself understand.
		raw := map[string]interface{}{}
		_ = yaml.Unmarshal(data, &raw)
		raw["server"] = map[string]interface{}{
			"host":          s.Server.Host,
			"port":          s.Server.Port,
			"trusted_proxy": s.Server.TrustedProxy,
			"title":         s.Server.Title,
			"tls_cert":      s.Server.TLSCert,
			"tls_key":       s.Server.TLSKey,
		}
		raw["auth"] = map[string]interface{}{
			"session_secret": s.Auth.SessionSecret,
			"session_ttl":    s.Auth.SessionTTL,
		}
		raw["sso"] = map[string]interface{}{
			"user_header": s.SSO.UserHeader,
			"hmac_secret": s.SSO.HMACSecret,
		}
		out, _ := yaml.Marshal(raw)
		os.WriteFile(path, out, 0600)
		fmt.Printf("[srcos] persisted generated session_secret to %s\n", path)
	}

	return &s, nil
}

func PersistServerConfig(path, host string, port int, trustedProxy, title, tlsCert, tlsKey string) error {
	// Load existing or create default
	var raw map[string]interface{}
	data, err := os.ReadFile(path)
	if err == nil {
		yaml.Unmarshal(data, &raw)
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}

	server, ok := raw["server"].(map[string]interface{})
	if !ok {
		server = make(map[string]interface{})
	}
	if host != "" {
		server["host"] = host
	}
	if port > 0 {
		server["port"] = port
	}
	if trustedProxy != "" {
		server["trusted_proxy"] = trustedProxy
	}
	if title != "" {
		server["title"] = title
	}
	if tlsCert != "" {
		server["tls_cert"] = tlsCert
	}
	if tlsKey != "" {
		server["tls_key"] = tlsKey
	}
	raw["server"] = server

	return writeStateFile(path, raw)
}

// PersistSSOConfig writes the sso section of state.yaml. Pass empty strings
// to disable SSO.
func PersistSSOConfig(path, userHeader, hmacSecret string) error {
	var raw map[string]interface{}
	data, err := os.ReadFile(path)
	if err == nil {
		yaml.Unmarshal(data, &raw)
	}
	if raw == nil {
		raw = make(map[string]interface{})
	}
	raw["sso"] = map[string]interface{}{
		"user_header": userHeader,
		"hmac_secret": hmacSecret,
	}
	return writeStateFile(path, raw)
}

func writeStateFile(path string, v interface{}) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	// 0600: state.yaml holds the session_secret.
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}
