// campaign_swarm.go implements the WS-A stub for SKU "campaign-swarm@v1".
//
// WS-A scope: emit one BillEvent (Units=1 — campaign-swarm is per-job
// fixed, so a single "campaign launched" counter, even at $0, exercises
// the Stripe wire) plus a terminal `phase: "complete"` entry. WS-C
// replaces this with the real multi-channel orchestrator fan-out (see
// docs/sku-execution-spec.md §"Per-SKU router specs").
package skus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// CampaignSwarmRouter is the SKU router for "campaign-swarm@v1". The
// WS-A implementation is a no-op stub; deps.Orchestrator / deps.Nodes /
// deps.Secrets remain nil-tolerant until WS-C wires them in.
type CampaignSwarmRouter struct {
	deps SKURunnerDeps
}

// NewCampaignSwarmRouter constructs a router wired to deps.
func NewCampaignSwarmRouter(deps SKURunnerDeps) *CampaignSwarmRouter {
	return &CampaignSwarmRouter{deps: deps}
}

// Dispatch implements rpc.SKURouter for the campaign-swarm SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: "S", Units: 1} — campaign-swarm is
//     per-job-fixed; the catalog Dispatcher (cloud-side) builds the
//     StripeRef key as "<sku.Name>:<Meter>" — so Meter MUST be the
//     tier letter (S/M/L), not a semantic counter name. WS-A defaults
//     to "S" (smallest tier); WS-C will pick the tier based on inputs
//     (channel count, max_outreach, etc.) and fill in OneShotCents
//     from the pricing tier.
//  2. Append a terminal `phase: "complete"` entry.
//  3. Return nil.
func (r *CampaignSwarmRouter) Dispatch(ctx context.Context, taskID, tenantID string, _ map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// 1. Billing seam.
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "campaign-swarm@v1",
			Meter:      "S", // tier letter; catalog Dispatcher builds StripeRef "campaign-swarm:S"
			Units:      1,
			OccurredAt: now(),
		})
	}

	// 2. Terminal entry.
	if r.deps.Ledger != nil {
		data, _ := json.Marshal(map[string]any{
			"outputs": map[string]any{
				"stub":              true,
				"note":              "WS-A stub; WS-C implements multi-channel fan-out",
				"posts_sent":        0,
				"channels_succeeded": []string{},
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
