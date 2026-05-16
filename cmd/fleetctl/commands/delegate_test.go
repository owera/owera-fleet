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
	"time"

	"github.com/owera/owera-fleet/internal/log"
	osh "github.com/owera/owera-fleet/internal/ssh"
)

// fakeDialer + fakeClient stand in for the real *ssh.Dialer / *ssh.Client
// pair so the test never touches the network or ~/.ssh.
type fakeDialer struct {
	dialErr error
	client  *fakeClient
}

func (f *fakeDialer) Dial(_ context.Context, _ osh.Target, _ ...osh.Option) (RemoteClient, error) {
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	return f.client, nil
}

type fakeClient struct {
	result osh.Result
	runErr error
	closed bool
}

func (f *fakeClient) Run(_ context.Context, _ string) (osh.Result, error) {
	return f.result, f.runErr
}

func (f *fakeClient) Close() error { f.closed = true; return nil }

// withTestNodesFile writes a nodes.txt fixture and points delegate at it.
// Returns a cleanup that restores the previous global.
func withTestNodesFile(t *testing.T, contents string) func() {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.txt")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write nodes: %v", err)
	}
	prev := delegateNodesPath
	delegateNodesPath = path
	return func() { delegateNodesPath = prev }
}

// resetDelegateFlags restores every package-level flag variable to its
// default. Run at the top of every test so the table cases don't leak
// state into each other.
func resetDelegateFlags() {
	delegateNode = ""
	delegateAll = false
	delegateRandom = false
	delegateCmd = ""
	delegateTimeout = 60 * time.Second
	delegateJSON = false
	delegateQuiet = false
	delegateAnyNode = false
}

func TestValidateDelegateFlags(t *testing.T) {
	cases := []struct {
		name    string
		setup   func()
		wantErr string
	}{
		{
			name:    "no mode flag",
			setup:   func() { delegateCmd = "uptime" },
			wantErr: "exactly one of --node, --all, or --random",
		},
		{
			name: "node + all is mutually exclusive",
			setup: func() {
				delegateCmd = "uptime"
				delegateNode = "hermes@claw1.local"
				delegateAll = true
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "node + random is mutually exclusive",
			setup: func() {
				delegateCmd = "uptime"
				delegateNode = "hermes@claw1.local"
				delegateRandom = true
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "all + random is mutually exclusive",
			setup: func() {
				delegateCmd = "uptime"
				delegateAll = true
				delegateRandom = true
			},
			wantErr: "mutually exclusive",
		},
		{
			name:    "missing --cmd with --all",
			setup:   func() { delegateAll = true },
			wantErr: "--cmd is required",
		},
		{
			name:    "missing --cmd with --node",
			setup:   func() { delegateNode = "hermes@claw1.local" },
			wantErr: "--cmd is required",
		},
		{
			name: "valid --node + --cmd",
			setup: func() {
				delegateNode = "hermes@claw1.local"
				delegateCmd = "uptime"
			},
			wantErr: "",
		},
		{
			name: "valid --all + --cmd",
			setup: func() {
				delegateAll = true
				delegateCmd = "uptime"
			},
			wantErr: "",
		},
		{
			name: "valid --random + --cmd",
			setup: func() {
				delegateRandom = true
				delegateCmd = "uptime"
			},
			wantErr: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetDelegateFlags()
			tc.setup()
			err := validateDelegateFlags()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDelegateJSONLEmission is the load-bearing test: it drives runDelegate
// end-to-end with fakes and asserts the JSONL line matches the documented
// schema.
func TestDelegateJSONLEmission(t *testing.T) {
	resetDelegateFlags()
	defer resetDelegateFlags()

	// Point the registry at a fixture so --any-node is not required.
	cleanupNodes := withTestNodesFile(t, "hermes@claw1.local\n")
	defer cleanupNodes()

	// Point the JSONL log at a temp file.
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "delegate.jsonl")
	prevLog := delegateLogPath
	delegateLogPath = logPath
	defer func() { delegateLogPath = prevLog }()

	// Substitute a fake dialer that returns a known Result.
	prevDialer := newDialer
	fake := &fakeDialer{client: &fakeClient{
		result: osh.Result{
			Stdout:     "Darwin claw1.local 24.5.0\n",
			Stderr:     "",
			ExitCode:   0,
			DurationMs: 42,
		},
	}}
	newDialer = func(_ ...osh.Option) Dialer { return fake }
	defer func() { newDialer = prevDialer }()

	// Configure flags as if the operator ran:
	//   fleetctl delegate --node hermes@claw1.local --cmd "uname -a" --quiet
	delegateNode = "hermes@claw1.local"
	delegateCmd = "uname -a"
	delegateQuiet = true

	if err := runDelegate(delegateCobraCmd, nil); err != nil {
		t.Fatalf("runDelegate: %v", err)
	}

	if !fake.client.closed {
		t.Errorf("expected client.Close() to be called")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) != 1 {
		t.Fatalf("expected 1 JSONL line, got %d: %q", len(lines), data)
	}

	var ev struct {
		Timestamp  string `json:"ts"`
		Node       string `json:"node"`
		Phase      string `json:"phase"`
		Action     string `json:"action"`
		Result     string `json:"result"`
		DurationMs int64  `json:"duration_ms"`
		StderrTail string `json:"stderr_tail,omitempty"`
	}
	if err := json.Unmarshal(lines[0], &ev); err != nil {
		t.Fatalf("unmarshal: %v\nline: %s", err, lines[0])
	}

	t.Logf("emitted JSONL: %s", lines[0])

	if ev.Node != "hermes@claw1.local" {
		t.Errorf("Node = %q, want %q", ev.Node, "hermes@claw1.local")
	}
	if ev.Phase != "delegate" {
		t.Errorf("Phase = %q, want %q", ev.Phase, "delegate")
	}
	if ev.Action != "uname -a" {
		t.Errorf("Action = %q, want %q", ev.Action, "uname -a")
	}
	if ev.Result != string(log.ResultOK) {
		t.Errorf("Result = %q, want %q", ev.Result, log.ResultOK)
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not RFC3339Nano: %v", ev.Timestamp, err)
	}
}

func TestDelegateActionTruncation(t *testing.T) {
	// 200-char command should clip to 80 in the action field.
	long := strings.Repeat("x", 200)
	got := truncateAction(long)
	if len(got) != delegateActionMax {
		t.Errorf("truncateAction len = %d, want %d", len(got), delegateActionMax)
	}
	// Newlines and tabs should be flattened.
	if got := truncateAction("a\nb\tc"); got != "a b c" {
		t.Errorf("truncateAction newlines: got %q, want %q", got, "a b c")
	}
}

func TestDelegateRegistryRejectsUnknownNode(t *testing.T) {
	resetDelegateFlags()
	defer resetDelegateFlags()

	cleanup := withTestNodesFile(t, "hermes@claw1.local\n")
	defer cleanup()

	logDir := t.TempDir()
	prevLog := delegateLogPath
	delegateLogPath = filepath.Join(logDir, "delegate.jsonl")
	defer func() { delegateLogPath = prevLog }()

	prevDialer := newDialer
	newDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{dialErr: errors.New("should not dial")}
	}
	defer func() { newDialer = prevDialer }()

	delegateNode = "hermes@unknown.local"
	delegateCmd = "uptime"
	delegateQuiet = true

	err := runDelegate(delegateCobraCmd, nil)
	if err == nil {
		t.Fatalf("expected error for off-registry node, got nil")
	}
	if !strings.Contains(err.Error(), "any-node") {
		t.Errorf("expected --any-node hint in error, got %v", err)
	}
}

func TestDelegateDialFailureLogsError(t *testing.T) {
	resetDelegateFlags()
	defer resetDelegateFlags()

	cleanup := withTestNodesFile(t, "hermes@claw1.local\n")
	defer cleanup()

	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "delegate.jsonl")
	prevLog := delegateLogPath
	delegateLogPath = logPath
	defer func() { delegateLogPath = prevLog }()

	prevDialer := newDialer
	newDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{dialErr: errors.New("connection refused")}
	}
	defer func() { newDialer = prevDialer }()

	delegateNode = "hermes@claw1.local"
	delegateCmd = "uptime"
	delegateQuiet = true

	err := runDelegate(delegateCobraCmd, nil)
	if err == nil {
		t.Fatalf("expected error when dial fails, got nil")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !bytes.Contains(data, []byte(`"result":"error"`)) {
		t.Errorf("expected result=error in log, got: %s", data)
	}
	if !bytes.Contains(data, []byte("connection refused")) {
		t.Errorf("expected stderr_tail to contain dial error, got: %s", data)
	}
}
