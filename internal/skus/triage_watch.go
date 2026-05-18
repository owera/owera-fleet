// triage_watch.go implements the WS-A stub for SKU "triage-watch@v1".
//
// WS-A scope: emit one zero-unit BillEvent (proves the
// router → ledger → outbox → Stripe wire). Unlike the campaign-swarm
// stub, triage-watch is a long-running / subscription SKU — Dispatch
// returns nil as soon as the (future) cronjob is installed and the
// task stays in the `running` state until cancelled. The router opts
// into this lifecycle by implementing rpc.LongRunningRouter (see
// LongRunning() below). WS-B replaces this with the real
// cronjob-driven Zendesk poller (see docs/sku-execution-spec.md
// §"Per-SKU router specs").
package skus

import (
	"context"
	"time"
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

// LongRunning implements rpc.LongRunningRouter and tells the
// SubmitJobHandler that this SKU installs a persistent worker
// (cronjob) at Dispatch time. The handler keeps the runregistry
// entry alive after Dispatch returns so fleet.CancelTask can tear the
// worker down later, and skips any synthesised terminal ledger entry
// — the task stays in `running` until cancelled.
//
// WS-B will install the actual cronjob (Zendesk poller) inside
// Dispatch and emit per-tick `progress` + `bill` entries while the
// task is in the `running` state. Cancellation via fleet.CancelTask
// is what writes the terminal `cancelled` entry.
//
// Resolves open contract question #3 in docs/sku-execution-spec.md.
func (r *TriageWatchRouter) LongRunning() bool { return true }

// Dispatch implements rpc.SKURouter for the triage-watch SKU.
//
// WS-A behaviour:
//  1. Emit one BillEvent{Meter: "tickets_handled", Units: 0} — a real
//     usage record at $0 so the Stripe path is exercised end-to-end.
//     The base-fee BillEvent fires immediately so the subscription
//     billing wire is proven even before WS-B installs the cronjob.
//  2. Return nil with no terminal ledger entry. The handler sees
//     LongRunning() == true and leaves the task in `running`; only
//     fleet.CancelTask (writing a `cancelled` entry, WS-B work)
//     terminates the lifecycle.
func (r *TriageWatchRouter) Dispatch(ctx context.Context, taskID, tenantID string, _ map[string]interface{}) error {
	now := r.deps.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	// Billing seam: emit a $0 metered event so the outbox flusher
	// creates a Stripe usage record on the customer's subscription.
	// Fires immediately even though the cronjob has not been
	// installed yet — the subscription base-fee event is decoupled
	// from per-tick overage events (WS-B will add per-tick emits).
	if r.deps.Billing != nil {
		_ = r.deps.Billing.Emit(ctx, taskID, tenantID, BillEvent{
			SKU:        "triage-watch@v1",
			Meter:      "tickets_handled",
			Units:      0,
			OccurredAt: now(),
		})
	}

	// No terminal `complete` entry — this is a long-running SKU. The
	// task remains in `running` state until fleet.CancelTask is
	// invoked. WS-B will install a cronjob here that emits per-tick
	// `progress` + `bill` entries; WS-B's cancellation path will
	// write the terminal `cancelled` entry.
	return nil
}
