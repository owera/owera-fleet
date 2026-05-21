package commands

import (
	"context"
	"strings"
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

// TestSwarmRunnerStdoutTailFullPayloadUnderCap verifies a typical real-world
// JSON line (a few hundred bytes — far above the 80-byte command-label cap
// but well under the 4 KB output cap) survives intact. Regression guard
// against accidentally re-using truncateAction (a command-label trimmer) in
// place of tailBytes (the actual output-tail helper) for StdoutTail.
func TestSwarmRunnerStdoutTailFullPayloadUnderCap(t *testing.T) {
	prev := swarmDialer
	t.Cleanup(func() { swarmDialer = prev })

	// 256-byte realistic JSON payload — what a distributed benchmark or
	// ETL leaf actually emits. Picks a length comfortably above the
	// 80-byte command-label cap so the wrong helper would visibly cut it.
	payload := `{"host":"claw0","arch":"arm64","os":"26.5","brew_leaves":29,"brew_formulae":182,"load_1m":1.90,"uptime_days":1,"launchagents":0,"disk_used_pct":4,"hermes":"v0.14.0","details":{"swift":"6.0","python":"3.13","go":"1.23","node":"22"},"region":"local"}`
	if len(payload) <= 80 || len(payload) >= 4096 {
		t.Fatalf("test fixture size %d must be >80 and <4096", len(payload))
	}

	swarmDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{client: &fakeClient{
			result: osh.Result{Stdout: payload, ExitCode: 0},
		}}
	}

	runner := makeSwarmRunner(5 * time.Second)
	entries, err := runner(context.Background(), orchestrator.LeafInput{
		Node: "hermes@claw0.local", TaskID: "task-test", LeafID: "leaf-01",
		Payload: map[string]any{"cmd": "baseline.sh", "user": "hermes", "host": "claw0.local"},
	})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	if entries[0].StdoutTail != payload {
		t.Errorf("payload truncated:\n  got  = %q (%d bytes)\n  want = %q (%d bytes)",
			entries[0].StdoutTail, len(entries[0].StdoutTail), payload, len(payload))
	}
}

// TestSwarmRunnerStdoutTailKeepsLastBytesAboveCap verifies that on payloads
// larger than 4 KB, the LAST 4 KB is what survives (matching delegate's
// stderr policy: most-recent output is the most diagnostically useful when
// the tail has to be truncated).
func TestSwarmRunnerStdoutTailKeepsLastBytesAboveCap(t *testing.T) {
	prev := swarmDialer
	t.Cleanup(func() { swarmDialer = prev })

	// 8 KB of stdout: 4 KB of filler "A"s followed by 4 KB of "B"s.
	// After tail-truncation to 4 KB, only the "B"s should remain.
	filler := strings.Repeat("A", 4096)
	keepMe := strings.Repeat("B", 4096)
	payload := filler + keepMe

	swarmDialer = func(_ ...osh.Option) Dialer {
		return &fakeDialer{client: &fakeClient{
			result: osh.Result{Stdout: payload, ExitCode: 0},
		}}
	}

	runner := makeSwarmRunner(5 * time.Second)
	entries, _ := runner(context.Background(), orchestrator.LeafInput{
		Node: "hermes@claw0.local", TaskID: "task-test", LeafID: "leaf-01",
		Payload: map[string]any{"cmd": "noisy", "user": "hermes", "host": "claw0.local"},
	})
	got := entries[0].StdoutTail
	if len(got) != 4096 {
		t.Errorf("StdoutTail len = %d, want 4096", len(got))
	}
	if got != keepMe {
		t.Errorf("StdoutTail did not keep last 4 KB (head=%q tail=%q)",
			got[:32], got[len(got)-32:])
	}
}
