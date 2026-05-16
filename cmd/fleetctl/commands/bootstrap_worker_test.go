package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/bootstrap"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

func resetBWFlags() {
	bwNode = ""
	bwPhase = ""
	bwScriptDir = ""
	bwDryRun = false
	bwQuiet = true
	bwTimeout = 10 * time.Second
}

func TestBootstrapWorkerRequiresNode(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()

	logDir := t.TempDir()
	prevLog := bwLogPath
	bwLogPath = filepath.Join(logDir, "bootstrap.jsonl")
	defer func() { bwLogPath = prevLog }()

	if err := runBootstrapWorker(bootstrapWorkerCmd, nil); err == nil {
		t.Fatal("expected error for missing --node, got nil")
	} else if !strings.Contains(err.Error(), "--node") {
		t.Errorf("error %q should mention '--node'", err.Error())
	}
}

func TestBootstrapWorkerPhaseOK(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()
	bwNode = "hermes@claw1.local"
	bwPhase = "phase00_brew_baseline.sh"
	bwQuiet = true

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "bootstrap.jsonl")
	prevLog := bwLogPath
	bwLogPath = logPath
	defer func() { bwLogPath = prevLog }()

	// Write a fake phase script so the Orchestrator finds it.
	scriptDir := t.TempDir()
	fakeScript := `{"ts":"2026-01-01T00:00:00Z","node":"claw1","phase":"phase00","action":"summary","result":"ok","duration_ms":42}`
	if err := os.WriteFile(filepath.Join(scriptDir, "phase00_brew_baseline.sh"), []byte("#!/bin/bash\necho '"+fakeScript+"' >&2\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	bwScriptDir = scriptDir

	// Inject a fake Orchestrator that succeeds.
	prev := bwOrchestratorFactory
	bwOrchestratorFactory = func(target osh.Target, _ time.Duration) (*bootstrap.Orchestrator, error) {
		return &bootstrap.Orchestrator{
			ScriptDir: scriptDir,
			Upload: func(_ context.Context, _, _ string) error { return nil },
			Run: func(_ context.Context, _ string) (string, string, int, error) {
				return "", fakeScript, 0, nil
			},
		}, nil
	}
	defer func() { bwOrchestratorFactory = prev }()

	if err := runBootstrapWorker(bootstrapWorkerCmd, nil); err != nil {
		t.Fatalf("runBootstrapWorker: %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var ev struct {
		Phase  string `json:"phase"`
		Action string `json:"action"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimRight(data, "\n"), &ev); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, data)
	}
	if ev.Phase != "bootstrap" {
		t.Errorf("phase = %q, want bootstrap", ev.Phase)
	}
	if ev.Result != "ok" {
		t.Errorf("result = %q, want ok", ev.Result)
	}
}

func TestBootstrapWorkerPhaseFail(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()
	bwNode = "hermes@claw1.local"
	bwPhase = "phase00_brew_baseline.sh"
	bwQuiet = true

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "bootstrap.jsonl")
	prevLog := bwLogPath
	bwLogPath = logPath
	defer func() { bwLogPath = prevLog }()

	scriptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptDir, "phase00_brew_baseline.sh"), []byte("#!/bin/bash\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	bwScriptDir = scriptDir

	prev := bwOrchestratorFactory
	bwOrchestratorFactory = func(target osh.Target, _ time.Duration) (*bootstrap.Orchestrator, error) {
		return &bootstrap.Orchestrator{
			ScriptDir: scriptDir,
			Upload:    func(_ context.Context, _, _ string) error { return nil },
			Run: func(_ context.Context, _ string) (string, string, int, error) {
				errLine := `{"ts":"2026-01-01T00:00:00Z","node":"claw1","phase":"phase00","action":"brew_present","result":"error","duration_ms":5}`
				return "", errLine, 2, nil
			},
		}, nil
	}
	defer func() { bwOrchestratorFactory = prev }()

	err := runBootstrapWorker(bootstrapWorkerCmd, nil)
	if err == nil {
		t.Fatal("expected error for phase failure, got nil")
	}
	if !strings.Contains(err.Error(), "phase00") {
		t.Errorf("error %q should mention phase name", err.Error())
	}

	// Verify JSONL logged result=error.
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if !bytes.Contains(data, []byte(`"result":"error"`)) {
		t.Errorf("expected result=error in log: %s", data)
	}
}

func TestBootstrapWorkerOrchestratorDialError(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()
	bwNode = "hermes@claw1.local"
	bwQuiet = true

	logDir := t.TempDir()
	prevLog := bwLogPath
	bwLogPath = filepath.Join(logDir, "bootstrap.jsonl")
	defer func() { bwLogPath = prevLog }()

	prev := bwOrchestratorFactory
	bwOrchestratorFactory = func(_ osh.Target, _ time.Duration) (*bootstrap.Orchestrator, error) {
		return nil, fmt.Errorf("dial failed")
	}
	defer func() { bwOrchestratorFactory = prev }()

	if err := runBootstrapWorker(bootstrapWorkerCmd, nil); err == nil {
		t.Fatal("expected error when dial fails")
	}
}
