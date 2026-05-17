package commands

import (
	"strings"
	"testing"
)

// TestServeCommandRegistered keeps the wiring honest: the cobra subcommand
// has to be reachable from the root for `fleetctl serve` to work. If
// somebody removes the rootCmd.AddCommand line in serve.go's init(), this
// test catches it.
func TestServeCommandRegistered(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "serve" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("serve subcommand is not registered on rootCmd")
	}
}

// TestServeSkillRegistered mirrors the SKILL.md generation pipeline.
func TestServeSkillRegistered(t *testing.T) {
	fn, ok := skillRegistry["serve"]
	if !ok {
		t.Fatal("serve skill not registered")
	}
	s := fn()
	if s.Name != "serve" {
		t.Fatalf("expected name=serve, got %q", s.Name)
	}
	if !strings.Contains(s.Summary, "JSON-RPC") {
		t.Fatalf("expected summary to mention JSON-RPC, got %q", s.Summary)
	}
}

// TestServeDefaultBindIsLoopback documents the security-sensitive default:
// production gates the port behind a Cloudflare tunnel, so an accidental
// `0.0.0.0` default would silently expose the operator plane.
func TestServeDefaultBindIsLoopback(t *testing.T) {
	flag := serveCmd.Flag("addr")
	if flag == nil {
		t.Fatal("--addr flag missing")
	}
	if !strings.HasPrefix(flag.DefValue, "127.0.0.1") {
		t.Fatalf("expected loopback default, got %q", flag.DefValue)
	}
}
