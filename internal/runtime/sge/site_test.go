package sge

import (
	"os"
	"testing"

	"github.com/seqyuan/srcos/internal/config"
)

func TestFromStateDefaultsToTunnel(t *testing.T) {
	cfg, err := FromState(config.SGEState{
		Enabled: true, SubmitDir: "/shared/sub", RendezvousDir: "/shared/rd",
		PE: "smp", PEAccounting: "cores", PollSeconds: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Tunnel {
		t.Error("the tunnel must be on by default")
	}
	if cfg.SubmitDir != "/shared/sub" || cfg.Scheduler.PE != "smp" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.PollEvery.Seconds() != 3 {
		t.Errorf("poll = %v", cfg.PollEvery)
	}
}

func TestFromStateHonoursExplicitTunnelOff(t *testing.T) {
	off := false
	cfg, err := FromState(config.SGEState{
		Enabled: true, SubmitDir: "/s", RendezvousDir: "/r", PE: "smp", Tunnel: &off,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Tunnel {
		t.Error("tunnel: false was ignored")
	}
}

func TestFromStateRejectsDirectDial(t *testing.T) {
	// The route layer only accepts loopback, so DirectDial cannot work while the
	// tunnel is on; the site must say `tunnel: false` explicitly.
	if _, err := FromState(config.SGEState{
		Enabled: true, SubmitDir: "/s", RendezvousDir: "/r", PE: "smp", DirectDial: true,
	}); err == nil {
		t.Fatal("DirectDial with the default tunnel on must be rejected")
	}
}

func TestFromStateFileIsSilentWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	b, ok, err := FromStateFile(dir)
	if err != nil || ok || b != nil {
		t.Fatalf("FromStateFile(empty) = %v, %v, %v", b, ok, err)
	}
	if _, err := os.Stat(config.StatePath(dir)); !os.IsNotExist(err) {
		t.Error("FromStateFile created state.yaml as a side effect")
	}
}

func TestFromStateFileBuildsWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	body := "server: {host: 127.0.0.1, port: 30152}\n" +
		"auth: {session_secret: abc, session_ttl: 3600}\n" +
		"sge: {enabled: true, submit_dir: /shared/sub, rendezvous_dir: /shared/rd, pe: smp}\n"
	if err := os.WriteFile(config.StatePath(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b, ok, err := FromStateFile(dir)
	if err != nil || !ok || b == nil {
		t.Fatalf("FromStateFile = %v, %v, %v", b, ok, err)
	}
	if b.Config.SubmitDir != "/shared/sub" || b.Config.Scheduler.PE != "smp" {
		t.Fatalf("backend config = %+v", b.Config)
	}
	if !b.Config.Tunnel {
		t.Error("tunnel not on by default")
	}
}

func TestFromStateFileStaysDisabledWhenEnabledIsFalse(t *testing.T) {
	dir := t.TempDir()
	body := "server: {host: 127.0.0.1, port: 30152}\n" +
		"auth: {session_secret: abc, session_ttl: 3600}\n" +
		"sge: {enabled: false}\n"
	if err := os.WriteFile(config.StatePath(dir), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := FromStateFile(dir); err != nil || ok {
		t.Fatalf("disabled sge registered: ok=%v err=%v", ok, err)
	}
}
