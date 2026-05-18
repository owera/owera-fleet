// code_audit.go implements the WS-A-style stub for SKU "code-audit@v1".
//
// WS-A scope: emit one zero-unit BillEvent (proves the
// router → ledger → outbox → Stripe wire) plus a terminal `phase:
// "complete"` entry so the cloud-side job state machine reaches
// `succeeded`. The real implementation is a daily-run cronjob with a
// cached AST + diff-only analysis between runs; that lands in Wave 11+
// alongside the H1 long-running router contract.
//
// Mirrors the triage-watch@v1 stub shape: monthly-subscription SKU
// emits the semantic meter name ("findings_reported") with Units=0
// rather than a tier letter. The Stripe Billing Meter that backs this
// (analogous to MeterTriageWatchTickets) lands in Wave 11 once the
// operator provisions live-mode products.
package skus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owera/owera-fleet/internal/ledger"
)

// CodeAuditRouter is the SKU router for "code-audit@v1". The stub uses
// only deps.Ledger, deps.Now, and deps.Billing. deps.CronManager will
// be populated when the real daily-run cronjob lands; until then the
// router is one-shot and returns immediately.
type CodeAuditRouter struct {
	deps SKURunnerDeps
}

// NewCodeAuditRouter constructs a router wired to deps.
func NewCodeAuditRouter(deps SKURunnerDeps) *CodeAuditRouter {
	return &CodeAuditRouter{deps: deps}
}

// Dispatch implements rpc.SKURouter for the code-audit SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: "findings_reported", Units: 0} — a
//     real usage record at $0 so the Stripe path is exercised
//     end-to-end. Code-audit is a monthly subscription with per-finding
//     overage; the base subscription is billed via the cloud-side
//     subscription pipeline, and this metered event is for findings
//     overage above tier inclusion.
//  2. Append a terminal `phase: "complete"` entry.
//  3. Return nil.
//
// TODO(Wave 11+): code-audit is meant to be a long-running subscription
// SKU (daily cronjob installed at Dispatch time, runs until cancelled).
// The stub returns nil immediately, which causes the handler to write
// the final entry for us. Future work needs either to not-return-nil-
// from-Dispatch or to add a "long-running" flag to rpc.SKURouter (H1's
// open contract work). The real implementation also wires deps.CronManager
// to schedule the daily run, deps.Secrets to resolve git auth, and
// performs cached AST + diff-only analysis between runs.
func (r *CodeAuditRouter) Dispatch(ctx context.Context, taskID, tenantID string, _ map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// 1. Billing seam: emit a $0 metered event for the findings overage
	//    meter so the outbox flusher creates a Stripe usage record on
	//    the customer's subscription.
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "code-audit@v1",
			Meter:      "findings_reported",
			Units:      0,
			OccurredAt: now(),
		})
	}

	// 2. Terminal entry.
	if r.deps.Ledger != nil {
		data, _ := json.Marshal(map[string]any{
			"outputs": map[string]any{
				"stub":               true,
				"note":               "WS-A stub; real audit is a daily cronjob with cached AST + diff-only analysis",
				"findings":           0,
				"severity_breakdown": map[string]int{"high": 0, "medium": 0, "low": 0},
				"audit_url":          "",
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
