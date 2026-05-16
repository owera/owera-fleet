package ssh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		raw     string
		want    Target
		wantErr bool
	}{
		{"hermes@claw1.local", Target{User: "hermes", Host: "claw1.local", Port: 22}, false},
		{"hermes@claw1.local:2222", Target{User: "hermes", Host: "claw1.local", Port: 2222}, false},
		{"  hermes@claw2.local  ", Target{User: "hermes", Host: "claw2.local", Port: 22}, false},
		{"claw1.local", Target{}, true},
		{"@claw1.local", Target{}, true},
		{"hermes@", Target{}, true},
		{"hermes@claw1.local:0", Target{}, true},
		{"hermes@claw1.local:abc", Target{}, true},
		{"hermes@claw1.local:99999", Target{}, true},
	}
	for _, tc := range cases {
		got, err := ParseTarget(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q): want error, got %+v", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): unexpected error %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", tc.raw, got, tc.want)
		}
	}
}

func TestTargetString(t *testing.T) {
	tgt := Target{User: "hermes", Host: "claw1.local", Port: 22}
	if got := tgt.String(); got != "hermes@claw1.local:22" {
		t.Errorf("String: %q", got)
	}
	if got := tgt.Address(); got != "claw1.local:22" {
		t.Errorf("Address: %q", got)
	}
}

func TestNewDialerDefaults(t *testing.T) {
	d := NewDialer()
	cfg := d.Config()
	if cfg.ConnectTimeout != DefaultConnectTimeout {
		t.Errorf("ConnectTimeout: %v", cfg.ConnectTimeout)
	}
	if cfg.ServerAliveInterval != DefaultServerAliveInterval {
		t.Errorf("ServerAliveInterval: %v", cfg.ServerAliveInterval)
	}
	if cfg.KnownHostsPath != DefaultKnownHostsPath {
		t.Errorf("KnownHostsPath: %q", cfg.KnownHostsPath)
	}
	if !cfg.AcceptNew {
		t.Errorf("AcceptNew default should be true")
	}
	if cfg.StrictHostKey {
		t.Errorf("StrictHostKey default should be false")
	}
}

func TestOptionsOverrideDefaults(t *testing.T) {
	d := NewDialer(
		WithConnectTimeout(99),
		WithServerAliveInterval(0),
		WithKnownHostsPath("/tmp/hosts"),
		WithStrictHostKey(),
		WithKeyFile("/tmp/key", []byte("secret")),
		WithAgentSocket("/tmp/agent.sock"),
	)
	cfg := d.Config()
	if cfg.ConnectTimeout != 99 {
		t.Errorf("ConnectTimeout: %v", cfg.ConnectTimeout)
	}
	if cfg.ServerAliveInterval != 0 {
		t.Errorf("ServerAliveInterval: %v", cfg.ServerAliveInterval)
	}
	if cfg.KnownHostsPath != "/tmp/hosts" {
		t.Errorf("KnownHostsPath: %q", cfg.KnownHostsPath)
	}
	if !cfg.StrictHostKey || cfg.AcceptNew {
		t.Errorf("strict flag combination: strict=%v acceptNew=%v", cfg.StrictHostKey, cfg.AcceptNew)
	}
	if cfg.KeyFile != "/tmp/key" {
		t.Errorf("KeyFile: %q", cfg.KeyFile)
	}
	if string(cfg.KeyPassphrase) != "secret" {
		t.Errorf("KeyPassphrase: %q", cfg.KeyPassphrase)
	}
	if cfg.AgentSocket != "/tmp/agent.sock" {
		t.Errorf("AgentSocket: %q", cfg.AgentSocket)
	}

	// Mutating the returned copy must not affect the dialer.
	cfg.KeyPassphrase[0] = 'X'
	if string(d.Config().KeyPassphrase) != "secret" {
		t.Errorf("Config() should return a defensive copy")
	}
}

func TestBuildAuthRequiresSomething(t *testing.T) {
	d := NewDialer(WithAgentSocket(""))
	_, _, err := d.buildAuth(d.cfg)
	if err == nil {
		t.Errorf("expected error when no key + no agent")
	}
}

func TestHostKeyCallbackSeedsMissingFile(t *testing.T) {
	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "known_hosts")

	d := NewDialer(WithKnownHostsPath(hostsPath))
	cfg := d.cfg
	_, err := d.buildHostKeyCallback(cfg, Target{User: "hermes", Host: "claw1.local", Port: 22})
	if err != nil {
		t.Fatalf("buildHostKeyCallback: %v", err)
	}
	if _, err := os.Stat(hostsPath); err != nil {
		t.Errorf("expected %s to be created", hostsPath)
	}
}

func TestHostKeyCallbackStrictRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	hostsPath := filepath.Join(dir, "known_hosts")

	d := NewDialer(WithKnownHostsPath(hostsPath), WithStrictHostKey())
	_, err := d.buildHostKeyCallback(d.cfg, Target{User: "hermes", Host: "claw1.local", Port: 22})
	if err == nil {
		t.Errorf("expected strict mode to refuse missing known_hosts")
	}
}

func TestExpandHomeFallbackOnError(t *testing.T) {
	d := NewDialer()
	d.resolveHome = func() (string, error) { return "", errors.New("no home for you") }
	_, err := d.expandHome("~/foo")
	if err == nil {
		t.Errorf("expected error when home resolution fails")
	}

	// Non-tilde paths pass through untouched.
	got, err := d.expandHome("/etc/hosts")
	if err != nil || got != "/etc/hosts" {
		t.Errorf("absolute path passthrough: got %q err %v", got, err)
	}
}

func TestTailWriter(t *testing.T) {
	tw := newTailWriter(10)
	if _, err := tw.Write([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if got := tw.String(); got != "ello world" {
		t.Errorf("tail after single write: %q", got)
	}
	if _, err := tw.Write([]byte(" again")); err != nil {
		t.Fatal(err)
	}
	if got := tw.String(); got != "orld again" {
		t.Errorf("tail after second write: %q", got)
	}
}

// TestLiveAgentSmoke is only run when OWERA_FLEET_LIVE_SSH is set. It
// connects to the live worker named by OWERA_FLEET_LIVE_NODE (default
// hermes@claw1.local) and runs `whoami`. Used to verify end-to-end
// connectivity against the real fleet; CI skips it.
func TestLiveAgentSmoke(t *testing.T) {
	if os.Getenv("OWERA_FLEET_LIVE_SSH") == "" {
		t.Skip("set OWERA_FLEET_LIVE_SSH=1 to run live SSH probe")
	}
	target := os.Getenv("OWERA_FLEET_LIVE_NODE")
	if target == "" {
		target = "hermes@claw1.local"
	}
	tgt, err := ParseTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	d := NewDialer()
	client, err := d.Dial(context.Background(), tgt)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	out, err := client.RunCapture(context.Background(), "whoami")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	out = strings.TrimSpace(out)
	if out != tgt.User {
		t.Errorf("whoami: got %q want %q", out, tgt.User)
	}
}
