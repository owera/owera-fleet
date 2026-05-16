package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/state"
)

func resetHealthFlags() {
	healthNode = ""
	healthAll = false
	healthJSON = false
	healthQuiet = true
	healthTimeout = 0
	healthHermesDir = ""
}

func TestHealthGatewayOnly(t *testing.T) {
	resetHealthFlags()
	defer resetHealthFlags()
	healthJSON = true
	healthQuiet = false

	// Override state collector with fake data.
	prev := healthStateCollect
	healthStateCollect = func(_ string, _ context.Context) ([]state.LaunchAgent, error) {
		return []state.LaunchAgent{
			{Label: "com.hermes.heartbeat", PID: "1234", LastExit: "0"},
			{Label: "com.hermes.backup", PID: "-", LastExit: "0"},
		}, nil
	}
	defer func() { healthStateCollect = prev }()

	prevHost := hostnameFunc
	hostnameFunc = func() (string, error) { return "testhost", nil }
	defer func() { hostnameFunc = prevHost }()

	// Point JSONL log at temp file.
	logDir := t.TempDir()
	prevLog := healthLogPath
	healthLogPath = filepath.Join(logDir, "health.jsonl")
	defer func() { healthLogPath = prevLog }()

	var buf bytes.Buffer
	healthCmd.SetOut(&buf)
	defer healthCmd.SetOut(nil)

	if err := runHealth(healthCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var results []NodeHealth
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("unmarshal JSON: %v\nraw: %s", err, buf.Bytes())
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (gateway only), got %d", len(results))
	}
	gw := results[0]
	if !strings.HasPrefix(gw.Node, "gateway:") {
		t.Errorf("node = %q, expected prefix 'gateway:'", gw.Node)
	}
	if len(gw.Checks) != 2 {
		t.Errorf("expected 2 checks, got %d", len(gw.Checks))
	}
	if gw.Checks[0].Name != "com.hermes.heartbeat" {
		t.Errorf("check[0].name = %q, want com.hermes.heartbeat", gw.Checks[0].Name)
	}
}

func TestHealthDialError(t *testing.T) {
	resetHealthFlags()
	defer resetHealthFlags()
	healthNode = "hermes@claw1.local"
	healthQuiet = true

	prev := healthStateCollect
	healthStateCollect = func(_ string, _ context.Context) ([]state.LaunchAgent, error) {
		return nil, nil
	}
	defer func() { healthStateCollect = prev }()

	prevDialer := healthDialer
	healthDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{dialErr: errors.New("connection refused")}
	}
	defer func() { healthDialer = prevDialer }()

	prevHost := hostnameFunc
	hostnameFunc = func() (string, error) { return "testhost", nil }
	defer func() { hostnameFunc = prevHost }()

	logDir := t.TempDir()
	prevLog := healthLogPath
	healthLogPath = filepath.Join(logDir, "health.jsonl")
	defer func() { healthLogPath = prevLog }()

	// runHealth should still succeed (per-node errors are non-fatal at top level).
	if err := runHealth(healthCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthJSONLEmission(t *testing.T) {
	resetHealthFlags()
	defer resetHealthFlags()
	healthQuiet = true

	prev := healthStateCollect
	healthStateCollect = func(_ string, _ context.Context) ([]state.LaunchAgent, error) {
		return []state.LaunchAgent{{Label: "com.hermes.heartbeat", PID: "1", LastExit: "0"}}, nil
	}
	defer func() { healthStateCollect = prev }()

	prevHost := hostnameFunc
	hostnameFunc = func() (string, error) { return "testhost", nil }
	defer func() { hostnameFunc = prevHost }()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "health.jsonl")
	prevLog := healthLogPath
	healthLogPath = logPath
	defer func() { healthLogPath = prevLog }()

	if err := runHealth(healthCmd, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify JSONL was written.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var ev struct {
		Phase  string `json:"phase"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &ev); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, data)
	}
	if ev.Phase != "health" {
		t.Errorf("phase = %q, want health", ev.Phase)
	}
	if !strings.HasPrefix(ev.Action, "probe:") {
		t.Errorf("action = %q, expected probe: prefix", ev.Action)
	}
}

func TestHealthMutualExclusion(t *testing.T) {
	resetHealthFlags()
	defer resetHealthFlags()
	healthNode = "hermes@claw1.local"
	healthAll = true

	if err := runHealth(healthCmd, nil); err == nil {
		t.Fatal("expected error for --node + --all, got nil")
	}
}

