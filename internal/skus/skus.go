// Package skus implements operator-plane SKU routers wired into
// fleet.SubmitJob via rpc.SubmitJobHandler.RegisterSKU. Each SKU is a
// thin glue layer that consumes a SubmitJob payload (validated cloud
// side), drives the relevant primitive (cronjob / orchestrator /
// alerter / etc.), and stamps a typed BillEvent into the signed ledger
// so the cloud-side outbox flusher can emit a Stripe usage record.
//
// SKU naming convention: V0 routers use the `@v1` suffix
// (e.g. "triage-watch@v1", "campaign-swarm@v1") to match the cloud
// catalog's OpenAPI-style versioning. The pre-existing
// "delegate.shell.v1" router keeps its dotted form for compatibility;
// new SKUs added here should follow `@v1`. Resolves open contract
// question #1 from docs/sku-execution-spec.md.
//
// Phasing (see docs/sku-execution-spec.md):
//
//   - WS-A (this file + sibling stub routers): registers triage-watch@v1
//     and campaign-swarm@v1 as billable but no-op stubs so the cloud
//     can complete an end-to-end paid round-trip against the operator
//     plane. Stripe sees a $0 usage record; the wire is proven.
//   - WS-B: implements the real triage-watch (Zendesk poller + cronjob).
//   - WS-C: implements the real campaign-swarm (multi-channel orchestrator
//     fan-out).
package skus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/owera/owera-fleet/internal/alerting"
	"github.com/owera/owera-fleet/internal/hermesjobs"
	"github.com/owera/owera-fleet/internal/ledger"
	"github.com/owera/owera-fleet/internal/nodes"
	"github.com/owera/owera-fleet/internal/orchestrator"
)

// SKURunnerDeps is the shared dependency bundle wired at server boot
// and passed into every SKU router constructor. Routers should treat
// any field they touch as potentially nil — WS-A stubs only use
// Ledger, Now, and Billing; the rest are populated as WS-B/C land.
type SKURunnerDeps struct {
	// Ledger is the signed JSONL audit trail. Routers append their own
	// progress + terminal entries here (every entry carries tenant_id).
	Ledger *ledger.Ledger

	// Now is the clock; overridable in tests.
	Now func() time.Time

	// Billing emits typed BillEvent records as `phase: "bill"` ledger
	// entries that the cloud-side outbox flusher consumes out of band.
	Billing BillingEmitter

	// --- WS-B / WS-C placeholders. nil-tolerant in WS-A stubs. ---

	// CronManager installs long-running cronjobs (used by triage-watch).
	CronManager *hermesjobs.Manager

	// Secrets resolves per-tenant credentials (Zendesk token, Twitter
	// API key, etc.). WS-B picks a concrete type; declared as `any` for
	// now so the deps struct stays stable while the resolver contract
	// is being finalized.
	Secrets any

	// Alerter fires PagerDuty/OpsGenie/ntfy alerts (used by triage-watch
	// when a ticket crosses the priority threshold).
	Alerter *alerting.Router

	// Orchestrator drives multi-leaf swarm fan-out (used by campaign-swarm).
	Orchestrator *orchestrator.Orchestrator

	// Nodes is the worker registry used by campaign-swarm to pick fan-out
	// targets.
	Nodes *nodes.Registry
}

// BillEvent is the typed payload routers emit into the ledger so the
// cloud-side billing.Service outbox flusher can translate it into a
// Stripe usage record. The wire shape is stable across V0 SKUs; the
// Meter + Units / OneShotCents fields encode whichever billing model
// the SKU uses.
type BillEvent struct {
	SKU          string    `json:"sku"`                      // "triage-watch@v1"
	Meter        string    `json:"meter"`                    // "tickets_handled" | "campaigns_launched"
	Units        int64     `json:"units"`                    // metered SKUs
	OneShotCents int64     `json:"one_shot_cents,omitempty"` // per-job-fixed SKUs
	OccurredAt   time.Time `json:"occurred_at"`
}

// BillingEmitter is the seam between a SKU router and the ledger so
// tests can record BillEvents without touching disk. The production
// implementation (LedgerBillingEmitter) persists each event as a
// `phase: "bill"` ledger entry.
type BillingEmitter interface {
	// Emit records ev for taskID under tenantID. Implementations MUST
	// stamp tenant_id + task_id onto the resulting ledger entry so the
	// cloud-side reconciler can attribute the event correctly.
	Emit(ctx context.Context, taskID, tenantID string, ev BillEvent) error
}

// LedgerBillingEmitter is the default BillingEmitter: it serialises the
// BillEvent into the Data field of a `phase: "bill"` ledger.Entry and
// appends it via Ledger.Append. The cloud's LedgerTailClient explicitly
// classifies `bill` entries as non-terminal so the job state machine
// keeps advancing.
type LedgerBillingEmitter struct {
	Ledger *ledger.Ledger

	// Now is overridable in tests.
	Now func() time.Time
}

// Emit implements BillingEmitter.
func (e *LedgerBillingEmitter) Emit(_ context.Context, taskID, tenantID string, ev BillEvent) error {
	if e.Ledger == nil {
		return nil
	}
	now := e.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return e.Ledger.Append(taskID, ledger.Entry{
		Ts:       now(),
		TaskID:   taskID,
		TenantID: tenantID,
		Phase:    "bill",
		Action:   ev.SKU,
		Result:   "emitted",
		Data:     data,
	})
}
