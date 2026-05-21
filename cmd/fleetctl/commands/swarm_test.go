package commands

import (
	"context"
	"testing"
	"time"

	osh "github.com/owera/owera-fleet/internal/ssh"
	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/orchestrator"
)

// TestSwarmRunnerCapturesStdout verifies that the swarm leaf runner records
// the remote command's stdout into the ledger entry's StdoutTail field. This
// is the load-bearing invariant for scatter-gather workloads (e.g. distributed
// benchmark JSON lines) where the operator needs the leaf's structured output
// reassembled from the parent ledger.
func TestSwarmRunnerCapturesStdout(t *testing.T) {
	prev := swarmDialer
	t.Cleanup(func() { swarmDialer = prev })

	const wantStdout = `{"host":"claw0","arch":"arm64","median_sec":0.262}`
	swarmDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{
			client: &fakeClient{
				result: osh.Result{
					Stdout:   wantStdout,
					Stderr:   "",
					ExitCode: 0,
				},
			},
		}
	}

	runner := makeSwarmRunner(5 * time.Second)
	entries, err := runner(context.Background(), orchestrator.LeafInput{
		Node:   "hermes@claw0.local",
		TaskID: "task-test",
		LeafID: "leaf-01",
		Payload: map[string]any{
			"cmd":  "swift run bench",
			"user": "hermes",
			"host": "claw0.local",
		},
	})
	if err != nil {
		t.Fatalf("runner returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.StdoutTail != wantStdout {
		t.Errorf("StdoutTail = %q, want %q", got.StdoutTail, wantStdout)
	}
	if got.Result != ledger.ResultOK {
		t.Errorf("Result = %q, want %q", got.Result, ledger.ResultOK)
	}
	if got.StderrTail != "" {
		t.Errorf("StderrTail = %q, want empty on success", got.StderrTail)
	}
}

// TestSwarmRunnerCapturesStdoutOnExitNonZero verifies that even when the leaf
// exits non-zero, stdout that was emitted before the failure is still
// preserved (along with stderr_tail).
func TestSwarmRunnerCapturesStdoutOnExitNonZero(t *testing.T) {
	prev := swarmDialer
	t.Cleanup(func() { swarmDialer = prev })

	const partialStdout = `progress: 50%`
	const stderrMsg = "boom"
	swarmDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{
			client: &fakeClient{
				result: osh.Result{
					Stdout:   partialStdout,
					Stderr:   stderrMsg,
					ExitCode: 1,
				},
			},
		}
	}

	runner := makeSwarmRunner(5 * time.Second)
	entries, err := runner(context.Background(), orchestrator.LeafInput{
		Node:   "hermes@claw0.local",
		TaskID: "task-test",
		LeafID: "leaf-01",
		Payload: map[string]any{
			"cmd":  "false",
			"user": "hermes",
			"host": "claw0.local",
		},
	})
	if err != nil {
		t.Fatalf("runner returned error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	got := entries[0]
	if got.StdoutTail != partialStdout {
		t.Errorf("StdoutTail = %q, want %q", got.StdoutTail, partialStdout)
	}
	if got.StderrTail != stderrMsg {
		t.Errorf("StderrTail = %q, want %q", got.StderrTail, stderrMsg)
	}
	if got.Result != ledger.ResultError {
		t.Errorf("Result = %q, want %q", got.Result, ledger.ResultError)
	}
}
