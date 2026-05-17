package orchestrator_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/orchestrator"
)

func TestExecuteAllSuccess(t *testing.T) {
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	o := &orchestrator.Orchestrator{
		Ledger: l,
		Run: func(_ context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
			return []ledger.Entry{{Phase: "leaf", Action: "work:" + in.LeafID, Result: ledger.ResultOK}}, nil
		},
	}
	plan := orchestrator.Plan{
		TaskID: "task-1",
		Leaves: []orchestrator.LeafInput{
			{Node: "n1", LeafID: "l1"},
			{Node: "n2", LeafID: "l2"},
		},
	}
	res, err := o.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.OK {
		t.Errorf("expected OK")
	}
	if len(res.Leaves) != 2 {
		t.Errorf("got %d leaves", len(res.Leaves))
	}

	entries, err := l.Read("task-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) < 6 { // start + 2 leaf entries + 2 leaf-ok + done = 6
		t.Errorf("expected ≥6 ledger entries, got %d", len(entries))
	}
}

func TestExecutePropagatesError(t *testing.T) {
	o := &orchestrator.Orchestrator{
		Run: func(_ context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
			if in.LeafID == "bad" {
				return nil, errors.New("leaf failed")
			}
			return nil, nil
		},
	}
	res, err := o.Execute(context.Background(), orchestrator.Plan{
		TaskID: "t",
		Leaves: []orchestrator.LeafInput{{LeafID: "good"}, {LeafID: "bad"}},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.OK {
		t.Error("expected OK=false")
	}
}

func TestRetryEach(t *testing.T) {
	var attempts int32
	o := &orchestrator.Orchestrator{
		Run: func(_ context.Context, _ orchestrator.LeafInput) ([]ledger.Entry, error) {
			n := atomic.AddInt32(&attempts, 1)
			if n < 3 {
				return nil, errors.New("transient")
			}
			return []ledger.Entry{{Phase: "p", Action: "a", Result: ledger.ResultOK}}, nil
		},
	}
	res, err := o.Execute(context.Background(), orchestrator.Plan{
		TaskID:    "t",
		Leaves:    []orchestrator.LeafInput{{LeafID: "x"}},
		RetryEach: 5,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.OK {
		t.Errorf("expected success after retries")
	}
}

func TestMaxParallel(t *testing.T) {
	var concurrent, peak int32
	o := &orchestrator.Orchestrator{
		Run: func(_ context.Context, _ orchestrator.LeafInput) ([]ledger.Entry, error) {
			c := atomic.AddInt32(&concurrent, 1)
			defer atomic.AddInt32(&concurrent, -1)
			for {
				p := atomic.LoadInt32(&peak)
				if c <= p || atomic.CompareAndSwapInt32(&peak, p, c) {
					break
				}
			}
			return nil, nil
		},
	}
	plan := orchestrator.Plan{
		TaskID:      "t",
		MaxParallel: 2,
		Leaves:      make([]orchestrator.LeafInput, 10),
	}
	for i := range plan.Leaves {
		plan.Leaves[i] = orchestrator.LeafInput{LeafID: string(rune('a' + i))}
	}
	_, err := o.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want ≤2", peak)
	}
}

// TestExecuteSurfacesLedgerAppendErrors — when the parent ledger rejects
// Append (here: opened read-only, so every Append returns an error),
// SwarmResult.LedgerErrs must collect those errors instead of silently
// discarding them. Pre-fix, every `_ = o.Ledger.Append(...)` swallowed
// the error and the swarm reported OK with a silently-truncated audit
// trail.
func TestExecuteSurfacesLedgerAppendErrors(t *testing.T) {
	dir := t.TempDir()
	// Bootstrap the signing keys, then re-open read-only — Append on a
	// read-only ledger returns "ledger: opened read-only" for every call.
	if _, err := ledger.Open(dir); err != nil {
		t.Fatalf("bootstrap ledger: %v", err)
	}
	readonly, err := ledger.OpenReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}

	o := &orchestrator.Orchestrator{
		Ledger: readonly,
		Run: func(_ context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
			return []ledger.Entry{{Phase: "leaf", Action: "work:" + in.LeafID, Result: ledger.ResultOK}}, nil
		},
	}
	plan := orchestrator.Plan{
		TaskID: "audit-loss",
		Leaves: []orchestrator.LeafInput{{Node: "n1", LeafID: "l1"}},
	}
	res, err := o.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Append happens for: start, 1 leaf entry, leaf-ok, done = 4 attempts.
	if len(res.LedgerErrs) != 4 {
		t.Errorf("LedgerErrs = %d, want 4 (start + 1 leaf entry + leaf-ok + done); errs=%v",
			len(res.LedgerErrs), res.LedgerErrs)
	}
	for _, e := range res.LedgerErrs {
		if e == nil || e.Error() == "" {
			t.Errorf("ledger err is empty: %v", e)
		}
	}
	// Per-leaf work succeeded; OK should still reflect that.
	if !res.OK {
		t.Errorf("expected OK=true (per-leaf work succeeded; only ledger Append failed)")
	}
}
