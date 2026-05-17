package skus

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// TestCampaignSwarmDispatchStub mirrors TestTriageWatchDispatchStub
// (in triage_watch_test.go) but for campaign-swarm@v1. The key
// differences are Meter = "campaigns_launched" and Units = 1 — campaign-
// swarm is per-job fixed, so the "one campaign launched" counter is
// always 1.
func TestCampaignSwarmDispatchStub(t *testing.T) {
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

	r := NewCampaignSwarmRouter(deps)
	const (
		taskID   = "task-camp-stub-1"
		tenantID = "ten_acme"
	)
	if err := r.Dispatch(context.Background(), taskID, tenantID, map[string]interface{}{
		"audience_segment_url": "https://example.com/seg/abc",
		"channels":             []string{"twitter", "linkedin", "email"},
		"post_count":           10,
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
	if got.Event.SKU != "campaign-swarm@v1" {
		t.Errorf("BillEvent.SKU: got %q, want %q", got.Event.SKU, "campaign-swarm@v1")
	}
	if got.Event.Meter != "campaigns_launched" {
		t.Errorf("BillEvent.Meter: got %q, want %q", got.Event.Meter, "campaigns_launched")
	}
	if got.Event.Units != 1 {
		t.Errorf("BillEvent.Units: got %d, want 1", got.Event.Units)
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

// TestCampaignSwarmDispatchWritesBillEntry mirrors the LedgerBillingEmitter
// end-to-end test in triage_watch_test.go: with the production emitter
// wired in we should see two entries (bill + complete), and the bill
// entry's Data should decode back to a BillEvent for campaign-swarm.
func TestCampaignSwarmDispatchWritesBillEntry(t *testing.T) {
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

	r := NewCampaignSwarmRouter(deps)
	const (
		taskID   = "task-camp-stub-2"
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
	if ev.SKU != "campaign-swarm@v1" || ev.Meter != "campaigns_launched" || ev.Units != 1 {
		t.Errorf("bill data round-trip mismatch: %+v", ev)
	}
}
