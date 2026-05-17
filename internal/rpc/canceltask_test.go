package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/orchestrator"
	"github.com/owera/owera-fleet/internal/rpc"
	"github.com/owera/owera-fleet/internal/runregistry"
)

// TestCancelTask_UnknownReturnsFalse covers the no-op case: the task isn't
// registered (already done, or never existed), so cancelled=false and no
// ledger write happens.
func TestCancelTask_UnknownReturnsFalse(t *testing.T) {
	params := mustMarshal(t, rpc.CancelTaskParams{TaskID: "nope-not-here"})
	res, err := rpc.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.Cancelled {
		t.Errorf("expected cancelled=false for unknown task, got true")
	}
}

func TestCancelTask_MissingTaskID(t *testing.T) {
	params := mustMarshal(t, rpc.CancelTaskParams{})
	_, err := rpc.Handle(context.Background(), params)
	if !errors.Is(err, rpc.ErrMissingTaskID) {
		t.Errorf("expected ErrMissingTaskID, got %v", err)
	}
}

func TestCancelTask_MalformedParams(t *testing.T) {
	_, err := rpc.Handle(context.Background(), json.RawMessage(`{"task_id":`))
	if err == nil {
		t.Fatalf("expected unmarshal error, got nil")
	}
}

// TestCancelTask_Integration is the acceptance test from the issue:
// submit a long-running plan, cancel it via the RPC handler, assert
// cancelled=true, then read the ledger and confirm no ResultBill entry
// was written.
func TestCancelTask_Integration(t *testing.T) {
	const taskID = "integration-cancel-001"

	dir := t.TempDir()
	l, err := ledger.Open(dir)
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}

	// Long-running leaf: blocks until ctx is cancelled, then returns
	// ctx.Err() (which the orchestrator treats as a leaf error, but
	// because ctx is cancelled it writes the swarm/cancel entry instead
	// of a leaf-error).
	leafStarted := make(chan struct{}, 1)
	runner := func(ctx context.Context, _ orchestrator.LeafInput) ([]ledger.Entry, error) {
		select {
		case leafStarted <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Second):
			// Safety net: this test must not hang the suite if the
			// cancel signal somehow doesn't propagate.
			return []ledger.Entry{{
				Phase:  "leaf",
				Action: "should-not-happen",
				Result: ledger.ResultOK,
			}}, nil
		}
	}

	o := &orchestrator.Orchestrator{Ledger: l, Run: runner}

	plan := orchestrator.Plan{
		TaskID: taskID,
		Leaves: []orchestrator.LeafInput{{Node: "n1", LeafID: "l1"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // belt-and-suspenders; the handler also cancels.
	runregistry.Register(taskID, cancel)
	defer runregistry.Unregister(taskID)

	type runOut struct {
		res *orchestrator.SwarmResult
		err error
	}
	done := make(chan runOut, 1)
	go func() {
		r, e := o.Execute(ctx, plan)
		done <- runOut{r, e}
	}()

	// Wait until the leaf is actually running before we cancel, otherwise
	// we'd race a finished-before-cancel scenario.
	select {
	case <-leafStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("leaf did not start within 2s")
	}

	// Acceptance: invoke the RPC handler.
	params := mustMarshal(t, rpc.CancelTaskParams{TaskID: taskID})
	res, err := rpc.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.Cancelled {
		t.Errorf("expected cancelled=true for running task, got false")
	}

	var out runOut
	select {
	case out = <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Execute did not return within 2s of cancel")
	}
	if out.err != nil {
		t.Fatalf("Execute returned error: %v", out.err)
	}
	if out.res == nil {
		t.Fatalf("Execute returned nil result")
	}
	if !out.res.Cancelled {
		t.Errorf("SwarmResult.Cancelled = false, want true")
	}

	// Acceptance: ledger has an explicit cancel entry and NO ResultBill.
	entries, err := l.Read(taskID)
	if err != nil {
		t.Fatalf("ledger.Read: %v", err)
	}

	var sawCancel bool
	for _, e := range entries {
		if e.Result == ledger.ResultBill {
			t.Errorf("ledger has unexpected ResultBill entry: %+v", e)
		}
		if e.Phase == "swarm" && e.Action == "cancel" {
			sawCancel = true
			if e.Result != orchestrator.ResultCancelled {
				t.Errorf("cancel entry result = %q, want %q", e.Result, orchestrator.ResultCancelled)
			}
		}
	}
	if !sawCancel {
		t.Errorf("expected an explicit swarm/cancel ledger entry, got entries: %+v", entries)
	}

	// Handler should be idempotent: a second call after the registration is
	// gone returns cancelled=false (it isn't in the registry any more).
	res2, err := rpc.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Handle (second call): %v", err)
	}
	if res2.Cancelled {
		t.Errorf("second cancel call: expected cancelled=false, got true")
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
