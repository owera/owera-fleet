# V0 SKU execution wiring — spec

> **Audience:** operator-plane engineers (you, future Claude sessions). Sister docs: [`docs/roadmap.md`](roadmap.md), [`docs/plan.md`](plan.md), the master plan [`knowing-all-you-now-calm-leaf.md`](../knowing-all-you-now-calm-leaf.md).
>
> **Status:** Spec, not yet implemented. Surfaced by the 2026-05-17 V0 end-to-end smoke (cloud → tunnel → operator round-trip works, but `fleet.SubmitJob` returns `unknown sku: triage-watch@v1` because no router is registered).

**Last updated:** 2026-05-17 (evening, post-V0-smoke).

## The gap

`fleet.SubmitJob` currently routes one SKU (`delegate.shell.v1`). The two V0 SKUs the cloud catalog publishes — `triage-watch@v1`, `campaign-swarm@v1` — return `CodeInvalidParams: unknown sku: …`. Result: the cloud → tunnel → operator round-trip works (proven in the 2026-05-17 V0 smoke), but no V0 SKU can reach `succeeded`, so the billing reconciler outbox never sees a `bill` event for a real job.

## What's already in place (don't reinvent)

- **`SKURouter`** interface at `internal/rpc/submitjob.go:63`: `Dispatch(ctx, taskID, tenantID, inputs) error`. Routers emit their own ledger entries with `TenantID` stamped.
- **`SubmitJobHandler.RegisterSKU(name, router)`** at `internal/rpc/submitjob.go:120`. Registration site for the live server is `cmd/fleetctl/commands/serve.go:94`.
- **Idempotency** via `cloud_job_id → task_id` map at `<ledgerDir>/.cloud-jobs.json`. Handled by the handler; routers don't have to think about it.
- **`fleet.CancelTask`** RPC + `internal/runregistry` for live task tracking — cancellation is wired generically, routers just need to honor `ctx.Done()`.
- **Phase-2 primitives** all exist + are unit-tested:
  - `internal/hermesjobs` (cron) + `commands/cronjob.go`
  - `internal/secrets` + `commands/run-with-secrets.go` (stdin-only credential delivery)
  - `internal/alerting` (PagerDuty + OpsGenie + ntfy backends)
  - `internal/orchestrator` + `commands/swarm.go` (multi-leaf fan-out + ledger merge)
  - `internal/ledger` (signed JSONL ledger per task-id, append + tail + replay)
- **Wave-9 e2e scenarios** `usecase33_ticket_triage.yaml` and `usecase32_marketing_swarm.yaml` already prove the primitives compose at the CLI level. The work below promotes them from "exercised by scenario" to "exercised by SKURouter against a customer payload."

## Architecture

### New package: `internal/skus/`

Each V0 SKU becomes its own file:

```
internal/skus/
├── skus.go              shared types: SKURunnerDeps, BillingEmitter, helpers
├── triage_watch.go      TriageWatchRouter
├── campaign_swarm.go    CampaignSwarmRouter
└── *_test.go            per-router unit tests against fake primitives
```

**`SKURunnerDeps` struct** wires the primitives once at server boot and is shared across all routers:

```go
type SKURunnerDeps struct {
    Ledger       *ledger.Store
    CronManager  *hermesjobs.Manager   // for long-running SKUs
    Secrets      secrets.Resolver      // .env.gpg → stdin
    Alerter      alerting.Router       // PagerDuty/ntfy/etc.
    Orchestrator *orchestrator.Swarm   // multi-leaf fan-out
    Nodes        nodes.Registry        // for swarm target selection
    Billing      BillingEmitter        // emits "bill" ledger entries
    Now          func() time.Time
}
```

### Billing-emission contract (the new seam)

Routers emit one or more `ledger.Entry` with `Phase: "bill"` and a typed payload that the cloud's outbox flusher picks up:

```go
type BillEvent struct {
    SKU          string    `json:"sku"`            // "triage-watch@v1"
    Meter        string    `json:"meter"`          // "campaigns_launched" or "tickets_handled"
    Units        int64     `json:"units"`          // for metered SKUs
    OneShotCents int64     `json:"one_shot_cents,omitempty"` // for per-job-fixed SKUs
    OccurredAt   time.Time `json:"occurred_at"`
}
```

The cloud's `LedgerTailClient` already pulls `bill` entries (fleet PR #8). Wiring is bottom-up:

- Router emits → `internal/ledger` persists → `fleet.LedgerTail` streams → cloud `billing.Service` outbox row → cloud outbox flusher (now live as of cloud PR #37) → Stripe.

This is the **only new wire contract**. Everything else reuses what's already shipped.

### Registration in `cmd/fleetctl/commands/serve.go`

Add after the existing `delegate.shell.v1` line (currently at `serve.go:94`):

```go
deps := skus.SKURunnerDeps{...}
submit.RegisterSKU("triage-watch@v1", skus.NewTriageWatchRouter(deps))
submit.RegisterSKU("campaign-swarm@v1", skus.NewCampaignSwarmRouter(deps))
```

## Per-SKU router specs

### `triage-watch@v1`

**Inputs** (from cloud schema, already validated cloud-side):

```json
{
  "queue_url": "https://acme.zendesk.com/api/v2/views/123/tickets.json",
  "priority_threshold": 8
}
```

**Behavior:**

1. Resolve Zendesk API token from `Secrets.GetForTenant(tenantID, "zendesk")`. If unset → emit `phase: "failed", result: "secret_missing"`, return.
2. Install a `hermesjobs` cronjob `triage-watch.<task_id>` with interval=120s (sane default; could be parameterized later).
3. Each tick:
   - Append `phase: "progress", action: "poll", result: "ok|err"` to the ledger.
   - On any ticket where `priority >= priority_threshold`: emit `Alerter.Fire(severity: "high", title: "Ticket #N escalated", …)` AND append `phase: "progress", action: "escalate", result: "fired"`.
   - For metered billing: append `BillEvent{Meter: "tickets_handled", Units: <count_this_tick>}` once per tick.
4. On `ctx.Done()` (cancellation via `fleet.CancelTask`): tear down the cronjob, emit `phase: "cancelled"`, return.
5. **Lifecycle:** `triage-watch` is a long-running SKU. The router returns `nil` from `Dispatch` as soon as the cronjob is installed; the cronjob runs until cancelled. Task status stays `running` indefinitely (subscription model). See open question 3 below.

**Billing model:** monthly subscription ($499 base) + per-ticket overage ($2/event). Base fires once via `BillEvent{OneShotCents: 49900, …}` at install time; overage fires per-tick on tickets above threshold.

### `campaign-swarm@v1`

**Inputs** (cloud schema — check `api/internal/catalog/campaign_swarm.go` in `owera-cloud` for canonical shape):

```json
{
  "audience_segment_url": "...",
  "channels": ["twitter", "linkedin", "email"],
  "post_count": 10
}
```

**Behavior:**

1. Resolve per-channel credentials via `Secrets.GetForTenant(tenantID, "twitter")`, `…/linkedin`, `…/sendgrid`.
2. Build an `orchestrator.Plan` with one leaf per channel; targets chosen from `Nodes.Random(1)` (or all available workers if `swarm` is wired).
3. Dispatch via `Orchestrator.Swarm(plan)`. The orchestrator already merges per-worker ledgers under the same `task_id`.
4. On each leaf completion: append `phase: "progress", action: "channel_done", result: "ok|err"`.
5. When all leaves terminal: emit `BillEvent{OneShotCents: <tier_price>, Meter: "campaigns_launched", Units: 1}` then `phase: "complete"` with `outputs: {posts_sent: N, channels_succeeded: [...], errors: [...]}`.
6. **Lifecycle:** finite, ~5-15 min. Router returns when terminal entry written.

**Billing model:** per-campaign tiered fixed (S/M/L by channel count). One `BillEvent` per completion, no metered overage.

## Phasing (3 PRs)

| PR | Scope | Touch | Acceptance |
|---|---|---|---|
| **WS-A** Skeleton + billing seam | `internal/skus/skus.go` (deps + BillEvent + BillingEmitter); register `triage-watch@v1` + `campaign-swarm@v1` as **stub routers** that emit one `BillEvent{Meter, Units: 0}` + `phase: "complete"` immediately. Wire deps in `serve.go`. | new package + 1 file in commands | Submit triage-watch → status `succeeded` within seconds. Cloud outbox flusher emits a $0 Stripe usage record. Proves the ledger→outbox→Stripe path live. |
| **WS-B** Real triage-watch | Implement the cronjob install, Zendesk poller (mockable HTTP client), priority threshold + alert fire. Add `internal/skus/triage_watch_test.go` against a fake Zendesk via `httptest`. | `triage_watch.go` + 1 test | `fleetctl test --tier=e2e --scenario=triage-watch` (new): submit job with fake Zendesk URL, observe poll ledger entries, observe alert fire on synthetic high-priority ticket, billing event per tick. |
| **WS-C** Real campaign-swarm | Implement the multi-leaf orchestrator path against fake Twitter/LinkedIn/SendGrid HTTP clients. Wire credential resolver. Add unit + e2e tests. | `campaign_swarm.go` + tests | `fleetctl test --tier=e2e --scenario=campaign-swarm` (new): submit job, 3 leaves run concurrently, ledger merges correctly, `succeeded` with `outputs.posts_sent==N`. |

WS-A is ~half-day and is the **highest-leverage**: it converts the V0 smoke from "dispatched but failed" to "dispatched, succeeded, billed end-to-end" — which closes the **Phase-3 verification gate proper** (founding-plan verification step 10).

WS-B and WS-C are 2-3 days each; both gated on which paid external API credentials operator-side procures first. They can be developed against fakes and integrated when credentials arrive.

## Open contract questions (resolve before WS-A)

1. **Cloud catalog SKU naming.** Cloud sends `triage-watch@v1` (with `@v1` suffix). Operator's existing `delegate.shell.v1` uses dotted form. Pick one — recommend keeping `@v1` (closer to OpenAPI versioning convention). Trivial code change; document in `OPERATION.md`.
2. **Secrets storage.** `internal/secrets` reads `.env.gpg`. For per-tenant credentials we need either:
   - Per-tenant `.env.gpg` keyed by tenant_id, OR
   - Operator-side credential vault (Vault, AWS Secrets Manager, etc.).

   For V0: simplest is `.env.gpg` keyed `<tenant_id>__<service>` (e.g. `ten_6cQ5L0UvzkJ4TXCW__zendesk_api_token`). Document the convention; revisit if a paying customer asks for SSO-style secret rotation.
3. **Long-running task model.** ✅ **Resolved (H1, 2026-05-17).** Added the optional `rpc.LongRunningRouter` interface (`LongRunning() bool`); when a router satisfies it and returns true, `SubmitJobHandler` derives the dispatch ctx from `context.Background()` (so the persistent worker outlives the inbound request), keeps the `runregistry` entry alive after `Handle` returns, and skips any synthesised terminal ledger entry. `TriageWatchRouter` opts in; `CampaignSwarmRouter` and `ShellDelegateRouter` remain finite by default. Cancellation via `fleet.CancelTask` is the only path that writes a terminal entry for long-running SKUs.
4. **Cost-cap interaction.** Cloud already rejects with 402 if projected cost > cap (we hit this in the 2026-05-17 smoke). For long-running SKUs that bill monthly, the cap check uses `Pricer.ProjectMonthly(sku, inputs)` — that's already correct. No operator-side work.

## Test plan

End-to-end after WS-A + WS-B + WS-C land:

1. **WS-A live smoke:** submit `campaign-swarm` (or `triage-watch`) → status `succeeded` → Stripe test-mode `subscription_item.usage_record` for the tenant within 2 min of the outbox flusher tick.
2. **WS-B live smoke:** triage-watch with `MockZendeskURL`, verify 3 ticks of `progress` ledger entries + 1 simulated `escalate` → `alert.send_test` shows in `~/.hermes/logs/alerts.jsonl`.
3. **WS-C live smoke:** campaign-swarm with 3 channels on claw1+claw2; verify ledger has 3 leaf entries merged under one task_id.
4. **Cross-tenant isolation:** tenant A submits campaign-swarm; tenant B can't retrieve A's `task_id`'s ledger via `fleet.LedgerTail` (operator-side ACL probe; currently `tenant_id` is on every entry but `LedgerTail` doesn't filter — this audit might surface an additional small ticket).
5. **Cancellation:** submit triage-watch, wait 2 ticks, `POST /v1/jobs/{id}/cancel` → cronjob removed, terminal `cancelled` in ledger within 5s.

## What this spec doesn't cover

- **Real external API calls** (Zendesk, Twitter, LinkedIn, SendGrid). The routers should be implemented against interfaces so tests use fakes; integration with real APIs is per-customer/per-credentials work and outside the engineering critical path.
- **SOC 2 evidence wiring** (audit log of every API call to external services). Belongs to CSE workstream in Phase 4.
- **V1 SKUs** (`research-brief`, `code-audit`). Same pattern, additional routers; out of scope until V0 ships.

## Recommended starting point

Start with **WS-A only**. It's ~half-day, unblocks the full Phase-3 verification gate, and gives you a real Stripe usage record to demonstrate the platform end-to-end. WS-B/C wait for the credential procurement clock (you'd need a Zendesk dev account, Twitter dev account, etc. — all operator actions).

## Cross-references

- Founding plan: [`knowing-all-you-now-calm-leaf.md`](../knowing-all-you-now-calm-leaf.md) §"Phase 3: Customer plane — components"
- Operator roadmap: [`docs/roadmap.md`](roadmap.md)
- Cloud roadmap: [`owera-cloud/docs/roadmap.md`](https://github.com/owera/owera-cloud/blob/main/docs/roadmap.md)
- Origin: 2026-05-17 V0 end-to-end smoke. Cloud → tunnel → operator round-trip verified; gap surfaced when `fleet.SubmitJob("triage-watch@v1", …)` returned `-32602: unknown sku`. Hermes-setup STATE.md captures the same observation.
