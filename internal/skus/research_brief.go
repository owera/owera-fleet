// research_brief.go implements the WS-A-style stub for SKU
// "research-brief@v1".
//
// WS-A scope: emit one BillEvent (Units=1 — research-brief is per-job-
// fixed, so a single "brief delivered" counter at $0 exercises the
// Stripe wire) plus a terminal `phase: "complete"` entry so the cloud-
// side job state machine reaches `succeeded`. The catalog Dispatcher
// (cloud-side) builds the StripeRef key as "<sku.Name>:<Meter>" — so
// Meter MUST be the tier letter (S/M/L), not a semantic counter name.
// WS-A defaults to "M" (the master-plan default depth); a later wave
// will pick the tier based on inputs.depth and fill OneShotCents from
// the pricing tier.
package skus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// ResearchBriefRouter is the SKU router for "research-brief@v1". The
// stub uses only deps.Ledger, deps.Now, and deps.Billing; the rest of
// SKURunnerDeps stays nil-tolerant until the real Hermes web/browser
// + structured-report renderer lands.
type ResearchBriefRouter struct {
	deps SKURunnerDeps
}

// NewResearchBriefRouter constructs a router wired to deps.
func NewResearchBriefRouter(deps SKURunnerDeps) *ResearchBriefRouter {
	return &ResearchBriefRouter{deps: deps}
}

// Dispatch implements rpc.SKURouter for the research-brief SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: "M", Units: 1} — per-job-fixed tier
//     letter consumed by the catalog Dispatcher at reconcile time.
//  2. Append a terminal `phase: "complete"` entry.
//  3. Return nil.
//
// TODO(post-WS-A): pick tier letter from inputs["depth"] (S/M/L) and
// fill OneShotCents from the pricing tier so the BillEvent carries
// the real chargeable amount. The current stub bills $0 — Stripe sees
// a usage record but no money moves.
func (r *ResearchBriefRouter) Dispatch(ctx context.Context, taskID, tenantID string, _ map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// 1. Billing seam: emit one $0 brief-delivered event so the outbox
	//    flusher creates a Stripe usage record on the customer's
	//    subscription. Meter is the tier letter, NOT "briefs_delivered"
	//    — the catalog Dispatcher builds StripeRef "research-brief:M".
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "research-brief@v1",
			Meter:      "M", // tier letter; default depth per master plan
			Units:      1,
			OccurredAt: now(),
		})
	}

	// 2. Terminal entry. Encoding outputs in Data matches the contract
	//    the cloud-side LedgerTailClient.extractOutputsAndError reads.
	if r.deps.Ledger != nil {
		data, _ := json.Marshal(map[string]any{
			"outputs": map[string]any{
				"stub":         true,
				"note":         "WS-A stub; real research uses Hermes web/browser + structured-report renderer",
				"depth":        "M",
				"citations":    []string{},
				"sections":     0,
				"brief_url":    "",
				"summary_text": "",
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
