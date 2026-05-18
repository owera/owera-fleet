package skus

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// TestSelectTier exercises the campaign-swarm tier-selection matrix as
// pinned in the H2 task brief. Policy (see campaign_swarm.go for the
// rationale): the dominant of (channel count, max_outreach) wins —
//   - S: 1 channel  AND max_outreach ≤ 100
//   - M: 2 channels OR  max_outreach 101–1000
//   - L: 3+ channels OR max_outreach > 1000
//
// Missing axes resolve to "smallest" (NOT the JSON-schema defaults), so
// nil / empty inputs map to S.
func TestSelectTier(t *testing.T) {
	tests := []struct {
		name   string
		inputs map[string]interface{}
		want   string
	}{
		{
			name: "single_channel_low_outreach_is_S",
			inputs: map[string]interface{}{
				"channels":     []string{"email"},
				"max_outreach": 50,
			},
			want: "S",
		},
		{
			name: "two_channels_mid_outreach_is_M",
			inputs: map[string]interface{}{
				"channels":     []string{"email", "linkedin"},
				"max_outreach": 500,
			},
			want: "M",
		},
		{
			name: "three_channels_high_outreach_is_L",
			inputs: map[string]interface{}{
				"channels":     []string{"email", "linkedin", "x"},
				"max_outreach": 5000,
			},
			want: "L",
		},
		{
			name: "nil_axes_is_S",
			inputs: map[string]interface{}{
				"channels":     nil,
				"max_outreach": nil,
			},
			want: "S",
		},
		{
			name:   "empty_inputs_is_S",
			inputs: map[string]interface{}{},
			want:   "S",
		},
		{
			name:   "nil_map_is_S",
			inputs: nil,
			want:   "S",
		},
		// Dominant-axis cases: the larger of (channel-tier, outreach-tier)
		// wins. These pin the policy so a future refactor can't quietly
		// drop the "OR" semantics.
		{
			name: "many_channels_low_outreach_is_L",
			inputs: map[string]interface{}{
				"channels":     []string{"email", "linkedin", "x", "phone"},
				"max_outreach": 10,
			},
			want: "L",
		},
		{
			name: "single_channel_huge_outreach_is_L",
			inputs: map[string]interface{}{
				"channels":     []string{"email"},
				"max_outreach": 9999,
			},
			want: "L",
		},
		{
			name: "two_channels_low_outreach_is_M",
			inputs: map[string]interface{}{
				"channels":     []string{"email", "linkedin"},
				"max_outreach": 1,
			},
			want: "M",
		},
		{
			name: "single_channel_borderline_M_outreach",
			inputs: map[string]interface{}{
				"channels":     []string{"email"},
				"max_outreach": 101,
			},
			want: "M",
		},
		{
			name: "single_channel_borderline_S_outreach",
			inputs: map[string]interface{}{
				"channels":     []string{"email"},
				"max_outreach": 100,
			},
			want: "S",
		},
		{
			name: "single_channel_borderline_L_outreach",
			inputs: map[string]interface{}{
				"channels":     []string{"email"},
				"max_outreach": 1001,
			},
			want: "L",
		},
		// JSON-wire shapes: []interface{} + float64 are what
		// encoding/json produces. These guarantee the operator-plane
		// stays compatible with how the cloud actually delivers inputs.
		{
			name: "json_wire_shape_M",
			inputs: map[string]interface{}{
				"channels":     []interface{}{"email", "linkedin"},
				"max_outreach": float64(500),
			},
			want: "M",
		},
		{
			name: "json_wire_shape_L",
			inputs: map[string]interface{}{
				"channels":     []interface{}{"email", "linkedin", "x"},
				"max_outreach": float64(5000),
			},
			want: "L",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectTier(tc.inputs); got != tc.want {
				t.Errorf("selectTier(%v) = %q, want %q", tc.inputs, got, tc.want)
			}
		})
	}
}

// TestCampaignSwarmDispatchStub mirrors TestTriageWatchDispatchStub
// (in triage_watch_test.go) but for campaign-swarm@v1. The key
// differences are Meter = a tier letter (catalog Dispatcher builds
// StripeRef "campaign-swarm:<tier>" from this) and Units = 1.
//
// The scenario below uses 3 channels + post_count: 10 (the latter
// unused by tier selection — only channels & max_outreach drive
// the tier), so the expected tier is L (3+ channels).
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
		"channels":             []string{"x", "linkedin", "email"},
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
	// 3 channels, no max_outreach → channel-axis says L.
	if got.Event.Meter != "L" {
		t.Errorf("BillEvent.Meter: got %q, want %q", got.Event.Meter, "L")
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
//
// Dispatched with nil inputs → S tier (the default-tier path).
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
	// Nil inputs → default S tier.
	if ev.SKU != "campaign-swarm@v1" || ev.Meter != "S" || ev.Units != 1 {
		t.Errorf("bill data round-trip mismatch: %+v", ev)
	}
	if !isValidTier(ev.Meter) {
		t.Errorf("bill Meter %q is not a valid tier letter (S/M/L)", ev.Meter)
	}
}

// TestCampaignSwarmDispatchTierEndToEnd round-trips a non-S tier through
// the production LedgerBillingEmitter so we know the picked tier letter
// actually lands in the signed ledger (and not just in the in-memory
// recording emitter used elsewhere).
func TestCampaignSwarmDispatchTierEndToEnd(t *testing.T) {
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
		taskID   = "task-camp-tier-L"
		tenantID = "ten_acme"
	)
	// 3 channels + 5000 outreach → both axes vote L.
	if err := r.Dispatch(context.Background(), taskID, tenantID, map[string]interface{}{
		"channels":     []string{"email", "linkedin", "x"},
		"max_outreach": 5000,
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	entries, err := led.Read(taskID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var bill *ledger.Entry
	for i := range entries {
		if entries[i].Phase == "bill" {
			bill = &entries[i]
			break
		}
	}
	if bill == nil {
		t.Fatalf("no `bill` ledger entry written")
	}
	var ev BillEvent
	if err := json.Unmarshal(bill.Data, &ev); err != nil {
		t.Fatalf("decode bill data: %v", err)
	}
	if ev.Meter != "L" {
		t.Errorf("bill Meter for 3-channel/5000-outreach campaign: got %q, want %q", ev.Meter, "L")
	}
}

func isValidTier(s string) bool {
	switch s {
	case "S", "M", "L":
		return true
	}
	return false
}
