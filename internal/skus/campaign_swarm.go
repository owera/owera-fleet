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

// selectTier picks the campaign-swarm pricing tier (S / M / L) from a
// validated inputs map. The chosen letter is the BillEvent.Meter value;
// the catalog Dispatcher (cloud-side) builds the StripeRef key as
// "campaign-swarm:<Meter>" and resolves it against StripeRefs in
// owera-cloud/api/internal/billing/stripe_ids.go ($499 / $999 / $1,999).
//
// Policy — the dominant of (channel count, max_outreach) wins, i.e. the
// caller is billed at the larger tier when one axis would say S and the
// other would say L:
//
//   - S: 1 channel  AND max_outreach ≤ 100
//   - M: 2 channels OR  max_outreach 101–1000
//   - L: 3+ channels OR max_outreach > 1000
//
// Missing axes are treated as "smallest", not as the catalog JSON-schema
// defaults. The cloud-side schema defaults (channels=["email"] →
// 1 channel; max_outreach=500) exist so the cloud Dispatcher has a
// concrete payload to fan out; they are *not* a signal that the caller
// asked for an M-tier campaign. A caller who says nothing pays the
// smallest tier (S); a caller upgrades only by explicitly supplying a
// value that crosses a threshold. This also matches the legacy WS-A
// behaviour of always emitting "S" when called with nil inputs.
//
// Inputs have already been validated against the cloud-side JSON schema
// (channels enum + min 1; max_outreach 1–10000) by the time the
// operator-plane router is invoked, so selectTier does not defensively
// re-validate ranges or enum membership.
func selectTier(inputs map[string]interface{}) string {
	channelTier := tierFromChannelCount(readChannelCount(inputs))
	outreachTier := tierFromMaxOutreach(readMaxOutreach(inputs))
	return maxTier(channelTier, outreachTier)
}

// readChannelCount returns the number of channels supplied in the inputs
// map, or 0 when the key is absent or malformed. JSON-unmarshalled maps
// land as []interface{}; tests sometimes pass []string. Both are accepted.
func readChannelCount(inputs map[string]interface{}) int {
	switch v := inputs["channels"].(type) {
	case []interface{}:
		return len(v)
	case []string:
		return len(v)
	}
	return 0
}

// readMaxOutreach returns the explicit max_outreach value, or 0 when the
// key is absent or malformed. encoding/json unmarshals integers as
// float64 by default; tests sometimes pass int directly. Both accepted.
func readMaxOutreach(inputs map[string]interface{}) int {
	switch v := inputs["max_outreach"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func tierFromChannelCount(n int) string {
	switch {
	case n >= 3:
		return "L"
	case n == 2:
		return "M"
	default:
		return "S"
	}
}

func tierFromMaxOutreach(n int) string {
	switch {
	case n > 1000:
		return "L"
	case n > 100:
		return "M"
	default:
		return "S"
	}
}

// maxTier returns whichever of two tier letters represents the larger
// tier (L > M > S).
func maxTier(a, b string) string {
	if tierRank(a) >= tierRank(b) {
		return a
	}
	return b
}

func tierRank(t string) int {
	switch t {
	case "L":
		return 2
	case "M":
		return 1
	default:
		return 0
	}
}

// Dispatch implements rpc.SKURouter for the campaign-swarm SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: <tier>, Units: 1} — campaign-swarm is
//     per-job-fixed; the catalog Dispatcher (cloud-side) builds the
//     StripeRef key as "<sku.Name>:<Meter>" — so Meter MUST be the
//     tier letter (S/M/L), not a semantic counter name. The tier is
//     picked from inputs by selectTier (see policy above); WS-C will
//     also fill in OneShotCents from the pricing tier.
//  2. Append a terminal `phase: "complete"` entry.
//  3. Return nil.
func (r *CampaignSwarmRouter) Dispatch(ctx context.Context, taskID, tenantID string, inputs map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	tier := selectTier(inputs)

	// 1. Billing seam.
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "campaign-swarm@v1",
			Meter:      tier, // tier letter; catalog Dispatcher builds StripeRef "campaign-swarm:<tier>"
			Units:      1,
			OccurredAt: now(),
		})
	}

	// 2. Terminal entry.
	if r.deps.Ledger != nil {
		data, _ := json.Marshal(map[string]any{
			"outputs": map[string]any{
				"stub":               true,
				"note":               "WS-A stub; WS-C implements multi-channel fan-out",
				"posts_sent":         0,
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
