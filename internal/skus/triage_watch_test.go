package skus

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// recordingBillingEmitter is a test double that records every BillEvent
// passed through Emit so we can assert on the call shape.
type recordingBillingEmitter struct {
	calls []recordedBillCall
}

type recordedBillCall struct {
	TaskID   string
	TenantID string
	Event    BillEvent
}

func (r *recordingBillingEmitter) Emit(_ context.Context, taskID, tenantID string, ev BillEvent) error {
	r.calls = append(r.calls, recordedBillCall{TaskID: taskID, TenantID: tenantID, Event: ev})
	return nil
}

// TestTriageWatchDispatchStub locks the WS-A contract: one BillEvent
// recorded with the expected meter + zero units, plus exactly one
// `bill` ledger entry and one `complete` ledger entry, both stamped
// with tenant_id + task_id.
//
// Note: this test uses a recordingBillingEmitter (NOT LedgerBillingEmitter)
// to inspect the BillEvent struct directly. The ledger-side assertion
// counts only the `complete` entry. A separate test (below) covers the
// LedgerBillingEmitter end-to-end so we know the `bill` entry actually
// hits the signed ledger in production wiring.
func TestTriageWatchDispatchStub(t *testing.T) {
	tmp := t.TempDir()
	led, err := ledger.Open(filepath.Join(tmp, "ledger"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	frozen := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	emitter := &recordingBillingEmitter{}
	deps := SKURunnerDeps{
		Ledger:  led,
		Now:     func() time.Time { return frozen },
		Billing: emitter,
	}

	r := NewTriageWatchRouter(deps)
	const (
		taskID   = "task-triage-stub-1"
		tenantID = "ten_acme"
	)
	if err := r.Dispatch(context.Background(), taskID, tenantID, map[string]interface{}{
		"queue_url":          "https://acme.zendesk.com/api/v2/views/123/tickets.json",
		"priority_threshold": 8,
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Exactly one BillEvent, with the expected shape.
	if got, want := len(emitter.calls), 1; got != want {
		t.Fatalf("BillEvent count: got %d, want %d", got, want)
	}
	got := emitter.calls[0]
	if got.TaskID != taskID || got.TenantID != tenantID {
		t.Errorf("BillEvent target: got task=%q tenant=%q, want task=%q tenant=%q",
			got.TaskID, got.TenantID, taskID, tenantID)
	}
	if got.Event.SKU != "triage-watch@v1" {
		t.Errorf("BillEvent.SKU: got %q, want %q", got.Event.SKU, "triage-watch@v1")
	}
	if got.Event.Meter != "tickets_handled" {
		t.Errorf("BillEvent.Meter: got %q, want %q", got.Event.Meter, "tickets_handled")
	}
	if got.Event.Units != 0 {
		t.Errorf("BillEvent.Units: got %d, want 0", got.Event.Units)
	}
	if !got.Event.OccurredAt.Equal(frozen) {
		t.Errorf("BillEvent.OccurredAt: got %v, want %v", got.Event.OccurredAt, frozen)
	}

	// Ledger should have exactly one `complete` entry (the recording
	// emitter swallows the would-be `bill` entry).
	entries, err := led.Read(taskID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger entry count: got %d, want 1; entries=%+v", len(entries), entries)
	}
	if entries[0].Phase != "complete" {
		t.Errorf("ledger[0].Phase: got %q, want %q", entries[0].Phase, "complete")
	}
	if entries[0].TenantID != tenantID || entries[0].TaskID != taskID {
		t.Errorf("ledger[0] tenancy: got task=%q tenant=%q, want task=%q tenant=%q",
			entries[0].TaskID, entries[0].TenantID, taskID, tenantID)
	}
}

// TestTriageWatchDispatchWritesBillEntry exercises the LedgerBillingEmitter
// path: with the production emitter wired in, Dispatch should leave two
// entries in the ledger — one `bill`, one `complete` — both stamped
// with the same tenant_id and task_id.
func TestTriageWatchDispatchWritesBillEntry(t *testing.T) {
	tmp := t.TempDir()
	led, err := ledger.Open(filepath.Join(tmp, "ledger"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}

	frozen := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	deps := SKURunnerDeps{
		Ledger:  led,
		Now:     func() time.Time { return frozen },
		Billing: &LedgerBillingEmitter{Ledger: led, Now: func() time.Time { return frozen }},
	}

	r := NewTriageWatchRouter(deps)
	const (
		taskID   = "task-triage-stub-2"
		tenantID = "ten_acme"
	)
	if err := r.Dispatch(context.Background(), taskID, tenantID, nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	entries, err := led.Read(taskID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ledger entry count: got %d, want 2; entries=%+v", len(entries), entries)
	}
	var bill, complete *ledger.Entry
	for i := range entries {
		e := &entries[i]
		switch e.Phase {
		case "bill":
			bill = e
		case "complete":
			complete = e
		}
	}
	if bill == nil {
		t.Fatalf("no `bill` ledger entry written")
	}
	if complete == nil {
		t.Fatalf("no `complete` ledger entry written")
	}
	for _, e := range []*ledger.Entry{bill, complete} {
		if e.TaskID != taskID || e.TenantID != tenantID {
			t.Errorf("entry %s missing tenancy: task=%q tenant=%q", e.Phase, e.TaskID, e.TenantID)
		}
	}

	// The bill entry's Data should round-trip into BillEvent.
	var ev BillEvent
	if err := json.Unmarshal(bill.Data, &ev); err != nil {
		t.Fatalf("decode bill data: %v", err)
	}
	if ev.SKU != "triage-watch@v1" || ev.Meter != "tickets_handled" || ev.Units != 0 {
		t.Errorf("bill data round-trip mismatch: %+v", ev)
	}
}
