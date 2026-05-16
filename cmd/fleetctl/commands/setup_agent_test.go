package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owera/owera-fleet/internal/launchd"
	"github.com/owera/owera-fleet/internal/log"
)

// fakeSetupMgr implements saManager for tests.
type fakeSetupMgr struct {
	installErr   error
	uninstallErr error
	statusResult launchd.Status
	statusErr    error
	installed    bool
	uninstalled  bool
}

func (f *fakeSetupMgr) Install(_ context.Context, _ []byte, _ string) error {
	f.installed = true
	return f.installErr
}

func (f *fakeSetupMgr) Uninstall(_ context.Context, _, _ string) error {
	f.uninstalled = true
	return f.uninstallErr
}

func (f *fakeSetupMgr) Status(_ context.Context, _ string) (launchd.Status, error) {
	return f.statusResult, f.statusErr
}

func (f *fakeSetupMgr) RenderTemplate(_ string, _ launchd.Vars) ([]byte, error) {
	return []byte("<plist/>"), nil
}

func resetSAFlags() {
	saInstall = false
	saUninstall = false
	saHermesDir = ""
	saRepoDir = ""
	saAlertURL = ""
	saResticRepo = ""
	saResticPassCmd = ""
	saUser = "hermes"
	saNodeLabel = ""
	saJSON = false
	saQuiet = true
}

func TestSetupAgentValidation(t *testing.T) {
	resetSAFlags()
	defer resetSAFlags()

	err := runSetupAgent(setupAgentCobraCmd, []string{"does-not-exist"})
	if err == nil {
		t.Fatal("expected error for unknown service, got nil")
	}
	if !strings.Contains(err.Error(), "unknown service") {
		t.Errorf("error %q should mention 'unknown service'", err.Error())
	}
}

func TestSetupAgentMutualExclusion(t *testing.T) {
	resetSAFlags()
	defer resetSAFlags()
	saInstall = true
	saUninstall = true

	err := runSetupAgent(setupAgentCobraCmd, []string{"heartbeat"})
	if err == nil {
		t.Fatal("expected error for --install + --uninstall together, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q should mention 'mutually exclusive'", err.Error())
	}
}

func TestSetupAgentStatusOK(t *testing.T) {
	resetSAFlags()
	defer resetSAFlags()
	saJSON = true
	saQuiet = false

	prev := newSAManager
	fake := &fakeSetupMgr{
		statusResult: launchd.Status{
			Label:  "com.hermes.heartbeat",
			Loaded: true,
			PID:    "1234",
			State:  "running",
		},
	}
	newSAManager = func(_ *log.Logger) saManager { return fake }
	defer func() { newSAManager = prev }()

	var buf bytes.Buffer
	setupAgentCobraCmd.SetOut(&buf)
	defer setupAgentCobraCmd.SetOut(nil)

	if err := runSetupAgent(setupAgentCobraCmd, []string{"heartbeat"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var out struct {
		Service string `json:"service"`
		Label   string `json:"label"`
		Loaded  bool   `json:"loaded"`
		State   string `json:"state"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, buf.Bytes())
	}
	if out.Service != "heartbeat" {
		t.Errorf("service = %q, want %q", out.Service, "heartbeat")
	}
	if !out.Loaded {
		t.Errorf("loaded = false, want true")
	}
	if out.State != "running" {
		t.Errorf("state = %q, want %q", out.State, "running")
	}
}

func TestSetupAgentInstallEmitsJSONL(t *testing.T) {
	resetSAFlags()
	defer resetSAFlags()
	saInstall = true
	saQuiet = true

	// Point log at a temp file.
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "setup-agent.jsonl")
	prevLog := setupAgentLogPath
	setupAgentLogPath = logPath
	defer func() { setupAgentLogPath = prevLog }()

	// Inject a fake home dir so PlistPath doesn't go near ~/Library.
	prevHome := userHomeDir
	fakeHome := t.TempDir()
	userHomeDir = func() (string, error) { return fakeHome, nil }
	defer func() { userHomeDir = prevHome }()

	prev := newSAManager
	fake := &fakeSetupMgr{}
	newSAManager = func(_ *log.Logger) saManager { return fake }
	defer func() { newSAManager = prev }()

	if err := runSetupAgent(setupAgentCobraCmd, []string{"heartbeat"}); err != nil {
		t.Fatalf("runSetupAgent: %v", err)
	}
	if !fake.installed {
		t.Error("expected Install() to be called")
	}

	// Verify JSONL was written and has correct schema.
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
		t.Fatalf("unmarshal JSONL: %v\nraw: %s", err, data)
	}
	if ev.Phase != "setup-agent" {
		t.Errorf("phase = %q, want %q", ev.Phase, "setup-agent")
	}
	if ev.Action != "install:heartbeat" {
		t.Errorf("action = %q, want %q", ev.Action, "install:heartbeat")
	}
	if ev.Result != string(log.ResultOK) {
		t.Errorf("result = %q, want %q", ev.Result, log.ResultOK)
	}
}

func TestSetupAgentUninstall(t *testing.T) {
	resetSAFlags()
	defer resetSAFlags()
	saUninstall = true
	saQuiet = true

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "setup-agent.jsonl")
	prevLog := setupAgentLogPath
	setupAgentLogPath = logPath
	defer func() { setupAgentLogPath = prevLog }()

	prevHome := userHomeDir
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	defer func() { userHomeDir = prevHome }()

	prev := newSAManager
	fake := &fakeSetupMgr{}
	newSAManager = func(_ *log.Logger) saManager { return fake }
	defer func() { newSAManager = prev }()

	if err := runSetupAgent(setupAgentCobraCmd, []string{"watchdog"}); err != nil {
		t.Fatalf("runSetupAgent: %v", err)
	}
	if !fake.uninstalled {
		t.Error("expected Uninstall() to be called")
	}
}
