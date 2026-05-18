package skus

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// TestCodeAuditDispatchStub locks the WS-A contract for the code-audit
// router: one BillEvent recorded with Meter="findings_reported" and
// Units=0 (stub model; the real findings count lands once the daily-run
// cronjob is implemented), plus exactly one `complete` ledger entry
// stamped with tenant_id + task_id. Mirrors triage-watch (the other
// monthly-subscription SKU).
func TestCodeAuditDispatchStub(t *testing.T) {
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

	r := NewCodeAuditRouter(deps)
	const (
		taskID   = "task-ca-stub-1"
		tenantID = "ten_acme"
	)
	if err := r.Dispatch(context.Background(), taskID, tenantID, map[string]interface{}{
		"repo_url":           "https://github.com/owera/owera-cloud",
		"branch":             "main",
		"severity_threshold": "medium",
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got, want := len(emitter.calls), 1; got != want {
		t.Fatalf("BillEvent count: got %d, want %d", got, want)
	}
	got := emitter.calls[0]
	if got.TaskID != taskID || got.TenantID != tenantID {
		t.Errorf("BillEvent target: got task=%q tenant=%q, want task=%q tenant=%q",
			got.TaskID, got.TenantID, taskID, tenantID)
	}
	if got.Event.SKU != "code-audit@v1" {
		t.Errorf("BillEvent.SKU: got %q, want %q", got.Event.SKU, "code-audit@v1")
	}
	if got.Event.Meter != "findings_reported" {
		t.Errorf("BillEvent.Meter: got %q, want %q", got.Event.Meter, "findings_reported")
	}
	if got.Event.Units != 0 {
		t.Errorf("BillEvent.Units: got %d, want 0", got.Event.Units)
	}
	if !got.Event.OccurredAt.Equal(frozen) {
		t.Errorf("BillEvent.OccurredAt: got %v, want %v", got.Event.OccurredAt, frozen)
	}

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

// TestCodeAuditDispatchWritesBillEntry exercises the LedgerBillingEmitter
// path: with the production emitter wired in, Dispatch should leave two
// entries in the ledger — one `bill`, one `complete` — both stamped
// with the same tenant_id and task_id.
func TestCodeAuditDispatchWritesBillEntry(t *testing.T) {
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

	r := NewCodeAuditRouter(deps)
	const (
		taskID   = "task-ca-stub-2"
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

	var ev BillEvent
	if err := json.Unmarshal(bill.Data, &ev); err != nil {
		t.Fatalf("decode bill data: %v", err)
	}
	if ev.SKU != "code-audit@v1" || ev.Meter != "findings_reported" || ev.Units != 0 {
		t.Errorf("bill data round-trip mismatch: %+v", ev)
	}
}
