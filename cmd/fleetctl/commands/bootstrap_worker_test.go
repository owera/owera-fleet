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
	bwPinnedVersion = ""
	bwPinnedFile = bwDefaultPinnedFile
	bwConfigBundleSrc = ""
	bwEnvGPG = bwDefaultEnvGPG
	bwSecretsWrapper = ""
	bwDedicatedKeyPub = bwDefaultDedicatedKey
	bwPlistTemplate = bwDefaultPlistTemplate
	bwGatewayPrefix = bwDefaultGatewayPrefix
	bwAllowExistAdmin = false
	bwSkipTirith = false
	bwSkipHeartbeat = false
	bwHeartbeatWaitSec = bwDefaultHeartbeatWait
	bwHermesUID = bwDefaultHermesUID
}

// TestPhasesDefault_TenInOrder asserts the canonical phase list is
// 10 scripts in numeric order (phase00 -> phase09). This is the
// load-bearing invariant of the wave-9 port: any reordering or
// dropped phase is a contract break that must be deliberate.
func TestPhasesDefault_TenInOrder(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()
	got := bwPhasesToRun()
	want := []string{
		"phase00_brew_baseline.sh",
		"phase01_verify_prereqs.sh",
		"phase02_create_user.sh",
		"phase03_install_hermes.sh",
		"phase04_seed_config.sh",
		"phase05_distribute_secrets.sh",
		"phase06_setup_dedicated_key.sh",
		"phase07_install_tirith.sh",
		"phase08_launchd_heartbeat.sh",
		"phase09_verify.sh",
	}
	if len(got) != len(want) {
		t.Fatalf("phase count: got %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("phase[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildPhasePrep_ArgShape exercises the per-phase argument shaping.
// It does not run the scripts — just asserts that for each phase, the
// generated args[] include the expected required flags (or that
// missing-input errors are returned). For phases that need staging,
// the prepare() closure is exercised against a fake upload.
func TestBuildPhasePrep_ArgShape(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()

	cases := []struct {
		phase      string
		setup      func(t *testing.T)
		wantFlags  []string // substrings that must appear in the joined args
		expectErr  bool
	}{
		{
			phase:     "phase00_brew_baseline.sh",
			wantFlags: []string{"--node", "u@h"},
		},
		{
			phase:     "phase01_verify_prereqs.sh",
			wantFlags: []string{"--node", "u@h"},
		},
		{
			phase: "phase02_create_user.sh",
			setup: func(t *testing.T) { bwAllowExistAdmin = true },
			wantFlags: []string{"--allow-existing-admin-user"},
		},
		{
			phase:     "phase03_install_hermes.sh",
			expectErr: true, // no pinned version provided
		},
		{
			phase: "phase07_install_tirith.sh",
			setup: func(t *testing.T) { bwSkipTirith = true },
			wantFlags: []string{"--skip"},
		},
		{
			phase: "phase08_launchd_heartbeat.sh",
			setup: func(t *testing.T) { bwSkipHeartbeat = true },
			wantFlags: []string{"--skip"},
		},
		{
			phase:     "phase09_verify.sh",
			expectErr: true, // no pinned version provided
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.phase, func(t *testing.T) {
			resetBWFlags()
			defer resetBWFlags()
			if tc.setup != nil {
				tc.setup(t)
			}
			prep, err := buildPhasePrep(tc.phase, "u@h", "")
			if tc.expectErr {
				if err == nil {
					t.Fatalf("%s: expected error, got nil (args=%v)", tc.phase, prep.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", tc.phase, err)
			}
			joined := strings.Join(prep.args, " ")
			for _, want := range tc.wantFlags {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: args missing %q (got: %s)", tc.phase, want, joined)
				}
			}
		})
	}
}

// TestBuildPhasePrep_HermesUID confirms that --uid is omitted from
// phase02 args when it matches the default (so existing call sites
// stay byte-identical), and propagates as "--uid N" when overridden.
// Motivation: claw-staging.local already had its admin account on
// UID 502, so hermes had to land at 503; phase02's underlying script
// has supported --uid since wave-9, but the runner never plumbed it.
func TestBuildPhasePrep_HermesUID(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()

	// Default UID: omitted from args.
	prep, err := buildPhasePrep("phase02_create_user.sh", "u@h", "")
	if err != nil {
		t.Fatalf("default-uid: %v", err)
	}
	if strings.Contains(strings.Join(prep.args, " "), "--uid") {
		t.Errorf("default UID should not emit --uid (got: %v)", prep.args)
	}

	// Override UID: propagates as "--uid 503".
	resetBWFlags()
	bwHermesUID = 503
	prep, err = buildPhasePrep("phase02_create_user.sh", "u@h", "")
	if err != nil {
		t.Fatalf("override-uid: %v", err)
	}
	joined := strings.Join(prep.args, " ")
	if !strings.Contains(joined, "--uid 503") {
		t.Errorf("override UID not in args (got: %s)", joined)
	}
}

// TestBuildPhasePrep_PinnedVersionPropagates confirms that when a
// pinned version is supplied, phase03 + phase09 both pick it up.
func TestBuildPhasePrep_PinnedVersionPropagates(t *testing.T) {
	resetBWFlags()
	defer resetBWFlags()

	for _, phase := range []string{"phase03_install_hermes.sh", "phase09_verify.sh"} {
		prep, err := buildPhasePrep(phase, "u@h", "v9.9.9")
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		joined := strings.Join(prep.args, " ")
		if !strings.Contains(joined, "--pinned-version v9.9.9") {
			t.Errorf("%s: pinned version not in args (got: %s)", phase, joined)
		}
	}
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
