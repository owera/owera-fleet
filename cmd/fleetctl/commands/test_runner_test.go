package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetTestFlags() {
	testScenario = nil
	testTier = 0
	testScenariosDir = "scenarios"
	testContinue = false
	testQuiet = true
	testJSON = false
	testTimeout = 0
}

func writeScenarioYAML(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// injectEchoRunner injects a fake stepRunner that succeeds for any cmd
// containing the trigger string, returning mockOut as stdout.
func injectEchoRunner(trigger, mockOut string) (restore func()) {
	prev := stepRunner
	stepRunner = func(_ context.Context, cmd string) (string, string, int, error) {
		if strings.Contains(cmd, trigger) || trigger == "*" {
			return mockOut, "", 0, nil
		}
		return "", "", 1, nil
	}
	return func() { stepRunner = prev }
}

func TestTestRunnerPassingScenario(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()

	scenDir := t.TempDir()
	writeScenarioYAML(t, scenDir, "pass.yaml", "name: pass\ntier: 1\ndescription: x\nsteps:\n  - name: echo\n    run: echo hello\n    expect:\n      exit: 0\n      stdout_contains: hello\n")
	testScenariosDir = scenDir

	testQuiet = false

	restore := injectEchoRunner("*", "hello\n")
	defer restore()

	logDir := t.TempDir()
	prevLog := testLogPath
	testLogPath = filepath.Join(logDir, "test.jsonl")
	defer func() { testLogPath = prevLog }()

	var buf bytes.Buffer
	testCmd.SetOut(&buf)
	defer testCmd.SetOut(nil)

	if err := runTest(testCmd, nil); err != nil {
		t.Fatalf("runTest: %v", err)
	}
	// Summary always appears in output; PASS per-scenario requires !quiet.
	if !strings.Contains(buf.String(), "passed") {
		t.Errorf("expected 'passed' in output: %s", buf.String())
	}
}

func TestTestRunnerFailingScenario(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()

	scenDir := t.TempDir()
	writeScenarioYAML(t, scenDir, "fail.yaml", "name: fail\ntier: 1\ndescription: x\nsteps:\n  - name: bad\n    run: exit 1\n    expect:\n      exit: 0\n")
	testScenariosDir = scenDir

	// Inject runner that returns exit 1.
	prev := stepRunner
	stepRunner = func(_ context.Context, _ string) (string, string, int, error) {
		return "", "", 1, nil
	}
	defer func() { stepRunner = prev }()

	logDir := t.TempDir()
	prevLog := testLogPath
	testLogPath = filepath.Join(logDir, "test.jsonl")
	defer func() { testLogPath = prevLog }()

	err := runTest(testCmd, nil)
	if err == nil {
		t.Fatal("expected error for failing scenario, got nil")
	}
}

func TestTestRunnerJSONLEmission(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()

	scenDir := t.TempDir()
	writeScenarioYAML(t, scenDir, "log.yaml", "name: log\ntier: 1\ndescription: x\nsteps:\n  - name: ok\n    run: echo ok\n    expect:\n      exit: 0\n")
	testScenariosDir = scenDir

	restore := injectEchoRunner("*", "ok\n")
	defer restore()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "test.jsonl")
	prevLog := testLogPath
	testLogPath = logPath
	defer func() { testLogPath = prevLog }()

	if err := runTest(testCmd, nil); err != nil {
		t.Fatalf("runTest: %v", err)
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
	if ev.Phase != "test" {
		t.Errorf("phase = %q, want test", ev.Phase)
	}
	if !strings.Contains(ev.Action, "log") {
		t.Errorf("action = %q should contain scenario name", ev.Action)
	}
	if ev.Result != "ok" {
		t.Errorf("result = %q, want ok", ev.Result)
	}
}

func TestTestRunnerTierFilter(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()

	scenDir := t.TempDir()
	writeScenarioYAML(t, scenDir, "t1.yaml", "name: t1\ntier: 1\ndescription: x\nsteps:\n  - name: ok\n    run: echo t1\n    expect:\n      exit: 0\n")
	writeScenarioYAML(t, scenDir, "t2.yaml", "name: t2\ntier: 2\ndescription: x\nsteps:\n  - name: fail\n    run: exit 1\n    expect:\n      exit: 0\n")
	testScenariosDir = scenDir
	testTier = 1

	// t1 always passes, t2 always fails.
	prev := stepRunner
	stepRunner = func(_ context.Context, cmd string) (string, string, int, error) {
		if strings.Contains(cmd, "t1") {
			return "t1\n", "", 0, nil
		}
		return "", "", 1, nil
	}
	defer func() { stepRunner = prev }()

	logDir := t.TempDir()
	prevLog := testLogPath
	testLogPath = filepath.Join(logDir, "test.jsonl")
	defer func() { testLogPath = prevLog }()

	// Tier 2 scenario (always fails) excluded → overall PASS.
	if err := runTest(testCmd, nil); err != nil {
		t.Fatalf("tier=1 filter should exclude failing t2 scenario: %v", err)
	}
}

func TestTestRunnerNoScenarios(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()
	testScenariosDir = t.TempDir()

	logDir := t.TempDir()
	prevLog := testLogPath
	testLogPath = filepath.Join(logDir, "test.jsonl")
	defer func() { testLogPath = prevLog }()

	if err := runTest(testCmd, nil); err == nil {
		t.Fatal("expected error for empty scenarios dir, got nil")
	}
}

func TestTestRunnerStdoutContainsCheck(t *testing.T) {
	resetTestFlags()
	defer resetTestFlags()

	scenDir := t.TempDir()
	writeScenarioYAML(t, scenDir, "check.yaml", "name: check\ntier: 1\ndescription: x\nsteps:\n  - name: verify\n    run: echo fleet-state\n    expect:\n      exit: 0\n      stdout_contains: fleet-state\n")
	testScenariosDir = scenDir

	prev := stepRunner
	stepRunner = func(_ context.Context, _ string) (string, string, int, error) {
		return "fleet-state\n", "", 0, nil
	}
	defer func() { stepRunner = prev }()

	logDir := t.TempDir()
	prevLog := testLogPath
	testLogPath = filepath.Join(logDir, "test.jsonl")
	defer func() { testLogPath = prevLog }()

	if err := runTest(testCmd, nil); err != nil {
		t.Fatalf("stdout_contains check failed: %v", err)
	}
}
