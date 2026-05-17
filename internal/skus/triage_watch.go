// triage_watch.go implements the WS-A stub for SKU "triage-watch@v1".
//
// WS-A scope: emit one zero-unit BillEvent (proves the
// router → ledger → outbox → Stripe wire) plus a terminal `phase:
// "complete"` entry so the cloud-side job state machine reaches
// `succeeded`. WS-B replaces this with the real cronjob-driven Zendesk
// poller (see docs/sku-execution-spec.md §"Per-SKU router specs").
package skus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// TriageWatchRouter is the SKU router for "triage-watch@v1". The WS-A
// implementation is a no-op stub; deps.CronManager / deps.Secrets /
// deps.Alerter are intentionally left untouched here and remain nil
// until WS-B wires them in.
type TriageWatchRouter struct {
	deps SKURunnerDeps
}

// NewTriageWatchRouter constructs a router wired to deps.
func NewTriageWatchRouter(deps SKURunnerDeps) *TriageWatchRouter {
	return &TriageWatchRouter{deps: deps}
}

// Dispatch implements rpc.SKURouter for the triage-watch SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: "tickets_handled", Units: 0} — a real
//     usage record at $0 so the Stripe path is exercised end-to-end.
//  2. Append a terminal `phase: "complete"` entry so the cloud-side
//     LedgerTailClient.mapPhaseToStatus → StatusSucceeded.
//  3. Return nil.
//
// TODO(WS-B): triage-watch is meant to be a long-running subscription
// SKU (cronjob installed at Dispatch time, runs until cancelled). The
// stub returns nil immediately, which causes the handler to write the
// final entry for us. WS-B will need either to not-return-nil-from-
// Dispatch or to add a "long-running" flag to rpc.SKURouter. Resolves
// open contract question #3 from docs/sku-execution-spec.md.
func (r *TriageWatchRouter) Dispatch(ctx context.Context, taskID, tenantID string, _ map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// 1. Billing seam: emit a $0 metered event so the outbox flusher
	//    creates a Stripe usage record on the customer's subscription.
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "triage-watch@v1",
			Meter:      "tickets_handled",
			Units:      0,
			OccurredAt: now(),
		})
	}

	// 2. Terminal entry. Encoding outputs in Data matches the contract
	//    the cloud-side LedgerTailClient.extractOutputsAndError reads.
	if r.deps.Ledger != nil {
		data, _ := json.Marshal(map[string]any{
			"outputs": map[string]any{
				"stub": true,
				"note": "WS-A stub; WS-B implements Zendesk poller",
			},
		})
		_ = r.deps.Ledger.Append(taskID, ledger.Entry{
			Ts:       now(),
			TaskID:   taskID,
			TenantID: tenantID,
			Phase:    "complete",
			Action:   "stub",
			Result:   ledger.ResultOK,
			Data:     data,
		})
	}

	return nil
}
