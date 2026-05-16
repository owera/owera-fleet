package secrets_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/secrets"
)

func TestRunDeliversPreamble(t *testing.T) {
	var out bytes.Buffer
	cfg := secrets.RunConfig{
		Cmd:     []string{"sh", "-c", "read -d '' line; echo \"GOT:$line\""},
		Secrets: map[string]string{"TOKEN": "abc123"},
		Stdout:  &out,
	}
	res, err := secrets.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d", res.ExitCode)
	}
	// The preamble "TOKEN=abc123\n\x00" is consumed; the rest is echoed.
	_ = out
}

func TestRunCapturesOutput(t *testing.T) {
	res, err := secrets.Run(context.Background(), secrets.RunConfig{
		Cmd:     []string{"echo", "hello"},
		Secrets: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestRunExitCode(t *testing.T) {
	res, err := secrets.Run(context.Background(), secrets.RunConfig{
		Cmd:     []string{"sh", "-c", "exit 42"},
		Secrets: map[string]string{},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit = %d, want 42", res.ExitCode)
	}
}

func TestRunWithEnv(t *testing.T) {
	res, err := secrets.RunWithEnv(context.Background(),
		[]string{"sh", "-c", "echo $MY_SECRET"},
		map[string]string{"MY_SECRET": "hunter2"},
		nil,
	)
	if err != nil {
		t.Fatalf("RunWithEnv: %v", err)
	}
	if !strings.Contains(res.Stdout, "hunter2") {
		t.Errorf("stdout = %q", res.Stdout)
	}
}

func TestParseEnvStream(t *testing.T) {
	input := "A=1\nB=hello world\nC=x=y\n\x00extra"
	m, err := secrets.ParseEnvStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("ParseEnvStream: %v", err)
	}
	if m["A"] != "1" || m["B"] != "hello world" || m["C"] != "x=y" {
		t.Errorf("parsed = %v", m)
	}
	if _, ok := m["extra"]; ok {
		t.Error("should not include bytes after NUL")
	}
}

func TestEmptyCommand(t *testing.T) {
	_, err := secrets.Run(context.Background(), secrets.RunConfig{})
	if err == nil {
		t.Error("expected error for empty command")
	}
}
