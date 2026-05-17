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

// TestPlanTenantIDPropagation guards the issue-#5 contract: Plan.TenantID
// stamps onto every ledger entry the orchestrator writes — both the parent
// swarm markers AND the merged per-leaf entries — so the customer plane can
// reconcile every signed row back to the tenant that owes the money.
func TestPlanTenantIDPropagation(t *testing.T) {
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	o := &orchestrator.Orchestrator{
		Ledger: l,
		Run: func(_ context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
			// Leaf returns its own per-leaf entry with TenantID UNSET; the
			// orchestrator must backfill it from the plan.
			return []ledger.Entry{{
				Phase:  "leaf",
				Action: "work:" + in.LeafID,
				Result: ledger.ResultOK,
			}}, nil
		},
	}
	plan := orchestrator.Plan{
		TaskID:    "tenant-task",
		TenantID:  "tenant-acme",
		ParentRun: "billing-run",
		Leaves: []orchestrator.LeafInput{
			{Node: "n1", LeafID: "l1"},
			{Node: "n2", LeafID: "l2"},
		},
	}
	if _, err := o.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	entries, err := l.Read("tenant-task")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries written")
	}
	// EVERY entry — parent markers + leaf entries — must carry the tenant.
	for i, e := range entries {
		if e.TenantID != "tenant-acme" {
			t.Errorf("entry[%d] (%s/%s): TenantID = %q, want tenant-acme",
				i, e.Phase, e.Action, e.TenantID)
		}
	}

	// And the cross-task tenant query surfaces them all.
	hits, err := l.ReadByTenant("tenant-acme")
	if err != nil {
		t.Fatalf("ReadByTenant: %v", err)
	}
	if len(hits) != len(entries) {
		t.Errorf("ReadByTenant returned %d, want %d", len(hits), len(entries))
	}
}

// TestPlanTenantIDDoesNotOverrideExplicitLeafValue documents the precedence
// rule: a leaf runner that explicitly sets a different TenantID on its
// returned entry is trusted (the orchestrator does NOT clobber it). This
// matters for future cases where a leaf might attribute work to a sub-tenant.
func TestPlanTenantIDDoesNotOverrideExplicitLeafValue(t *testing.T) {
	l, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	o := &orchestrator.Orchestrator{
		Ledger: l,
		Run: func(_ context.Context, in orchestrator.LeafInput) ([]ledger.Entry, error) {
			return []ledger.Entry{{
				TenantID: "tenant-sub",
				Phase:    "leaf",
				Action:   "work:" + in.LeafID,
				Result:   ledger.ResultOK,
			}}, nil
		},
	}
	plan := orchestrator.Plan{
		TaskID:   "parent-task",
		TenantID: "tenant-parent",
		Leaves:   []orchestrator.LeafInput{{Node: "n1", LeafID: "l1"}},
	}
	if _, err := o.Execute(context.Background(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	entries, err := l.Read("parent-task")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var leafEntries, parentEntries int
	for _, e := range entries {
		if e.Phase == "leaf" {
			leafEntries++
			if e.TenantID != "tenant-sub" {
				t.Errorf("leaf entry tenant clobbered: %q (want tenant-sub)", e.TenantID)
			}
		} else {
			parentEntries++
			if e.TenantID != "tenant-parent" {
				t.Errorf("parent entry tenant = %q (want tenant-parent)", e.TenantID)
			}
		}
	}
	if leafEntries == 0 || parentEntries == 0 {
		t.Errorf("got %d leaf / %d parent entries; expected both >0", leafEntries, parentEntries)
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
