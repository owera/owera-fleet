package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	osh "github.com/owera/owera-fleet/internal/ssh"
)

func resetUpdateFlags() {
	updateNode = ""
	updateAll = false
	updateDryRun = false
	updateVersion = ""
	updateQuiet = true
	updateTimeout = 0
}

func TestUpdateMutualExclusion(t *testing.T) {
	resetUpdateFlags()
	defer resetUpdateFlags()
	updateNode = "hermes@claw1.local"
	updateAll = true

	logDir := t.TempDir()
	prevLog := updateLogPath
	updateLogPath = filepath.Join(logDir, "update.jsonl")
	defer func() { updateLogPath = prevLog }()

	if err := runUpdate(updateCmd, nil); err == nil {
		t.Fatal("expected error for --node + --all, got nil")
	}
}

func TestUpdateDryRunEmitsJSONL(t *testing.T) {
	resetUpdateFlags()
	defer resetUpdateFlags()
	updateNode = "hermes@claw1.local"
	updateDryRun = true
	updateQuiet = true

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "update.jsonl")
	prevLog := updateLogPath
	updateLogPath = logPath
	defer func() { updateLogPath = prevLog }()

	prevDialer := updateDialer
	updateDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{client: &fakeClient{
			result: osh.Result{Stdout: "hermes version 0.13.0\n", ExitCode: 0},
		}}
	}
	defer func() { updateDialer = prevDialer }()

	if err := runUpdate(updateCmd, nil); err != nil {
		t.Fatalf("runUpdate: %v", err)
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
	if ev.Phase != "update" {
		t.Errorf("phase = %q, want update", ev.Phase)
	}
	if ev.Result != "ok" {
		t.Errorf("result = %q, want ok", ev.Result)
	}
}

func TestUpdateDialFailure(t *testing.T) {
	resetUpdateFlags()
	defer resetUpdateFlags()
	updateNode = "hermes@claw1.local"
	updateQuiet = true

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "update.jsonl")
	prevLog := updateLogPath
	updateLogPath = logPath
	defer func() { updateLogPath = prevLog }()

	prevDialer := updateDialer
	updateDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{dialErr: errors.New("connection refused")}
	}
	defer func() { updateDialer = prevDialer }()

	err := runUpdate(updateCmd, nil)
	if err == nil {
		t.Fatal("expected error when dial fails, got nil")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Contains(data, []byte(`"result":"error"`)) {
		t.Errorf("expected result=error in log: %s", data)
	}
}

func TestUpdateNoTargetsOK(t *testing.T) {
	resetUpdateFlags()
	defer resetUpdateFlags()
	// No --node or --all: resolveUpdateTargets returns nil, nil.

	logDir := t.TempDir()
	prevLog := updateLogPath
	updateLogPath = filepath.Join(logDir, "update.jsonl")
	defer func() { updateLogPath = prevLog }()

	if err := runUpdate(updateCmd, nil); err != nil {
		t.Fatalf("expected no error for empty target list, got: %v", err)
	}
}

func TestUpdateFlagValidation(t *testing.T) {
	resetUpdateFlags()
	defer resetUpdateFlags()
	updateNode = "hermes@claw1.local"
	updateAll = true

	logDir := t.TempDir()
	prevLog := updateLogPath
	updateLogPath = filepath.Join(logDir, "update.jsonl")
	defer func() { updateLogPath = prevLog }()

	err := runUpdate(updateCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("expected mutually exclusive error, got: %v", err)
	}
}
