package skus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// TestTriageWatchDispatchStub locks the WS-A + H1 contract: one BillEvent
// recorded with the expected meter + zero units, and NO terminal
// `complete` ledger entry (triage-watch is long-running; only
// fleet.CancelTask should write a terminal entry, in WS-B's cancellation
// path).
//
// Note: this test uses a recordingBillingEmitter (NOT
// LedgerBillingEmitter) to inspect the BillEvent struct directly. The
// ledger-side assertion checks that the stub does NOT add anything of
// its own to the ledger after dispatch returns — a separate test
// (below) covers the LedgerBillingEmitter end-to-end so we know the
// `bill` entry actually hits the signed ledger in production wiring.
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

	// H1 contract: triage-watch declares itself long-running so the
	// SubmitJobHandler keeps the runregistry entry alive and skips any
	// auto-terminal ledger entry.
	if !r.LongRunning() {
		t.Fatal("TriageWatchRouter.LongRunning() = false, want true")
	}

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

	// Ledger should be empty — recording emitter swallowed the would-be
	// `bill` entry, and the long-running router writes no terminal entry.
	// ledger.Read returns ENOENT when no entries were ever written; for
	// this contract assertion that's the SAME as "empty ledger", so we
	// accept ENOENT and otherwise demand zero entries.
	entries, err := led.Read(taskID)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read ledger: %v", err)
		}
		entries = nil
	}
	if len(entries) != 0 {
		t.Fatalf("ledger entry count: got %d, want 0 (long-running router must not write terminal entries); entries=%+v", len(entries), entries)
	}
}

// TestTriageWatchDispatchWritesBillEntry exercises the LedgerBillingEmitter
// path: with the production emitter wired in, Dispatch should leave
// exactly one entry in the ledger — the `bill` entry — stamped with the
// task_id + tenant_id. Crucially there is NO `complete` entry: the
// router is long-running and the task stays in `running` state until
// cancellation.
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
	if len(entries) != 1 {
		t.Fatalf("ledger entry count: got %d, want 1 (bill only, no complete for long-running); entries=%+v", len(entries), entries)
	}
	bill := entries[0]
	if bill.Phase != "bill" {
		t.Fatalf("ledger[0].Phase: got %q, want %q (long-running router must NOT write a terminal entry)", bill.Phase, "bill")
	}
	if bill.TaskID != taskID || bill.TenantID != tenantID {
		t.Errorf("bill entry missing tenancy: task=%q tenant=%q", bill.TaskID, bill.TenantID)
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
