# Owera-fleet & Owera-cloud — green-field recreation of hermes-setup to productize agentic work as a service

## Context

**The ultimate goal is a productized agentic-work service.** Owera Software offers Hermes-powered agentic capabilities (custom-app development, marketing swarms, ticket triage, ETL pipelines) to external clients on a paid basis. The current `hermes-setup` repo is the *operator-only* prototype of the engine that runs the work — a 23-script + 12-doc bash codebase that grew bottom-up through an 11-bug shake-down and now hosts a working 2-node fleet. The recreation is the bottom-up redesign that turns the prototype engine into a commercial product: a typed core, a real public API, a customer-facing dashboard, billing, SLAs, compliance trajectory.

**The current shape, honestly characterized:**

The current `hermes-setup` repo carries visible accidental complexity that blocks productization:

- **Inline mini-loggers, node enumeration, markdown report rendering, and JSON-escape logic are duplicated 5+ times** across scripts because there was no shared lib until late. `scripts/lib/` exists but is bootstrap-only.
- **Three test scripts** (`fleet-usecase-tests.sh`, `fleet-readiness-probes.sh`, `fleet-usecase-demos.sh`, ~3,700 lines combined) overlap in shape; tiers are conceptual, not encoded.
- **Five separate installers** (`install-backup.sh`, `install-backup-worker.sh`, `install-watchdog.sh`, `install-logrotate.sh`, `install-brew-on-worker.sh`) each duplicate plist-render + launchctl handshake; LaunchAgents are installed-but-not-loaded on the live gateway today.
- **`install-brew-on-worker.sh` is untracked** and was invoked as a one-off outside the bootstrap flow — a smell that worker provisioning isn't truly one command.
- **STATE.md and SECURITY_NOTES.md drift tables are hand-edited** even though both are derivable from the live system.
- **Four historical docs** (v1 plan, v1 review, single-container guide, data-prevention guide, important-notes) clutter the root and contradict the current state in places.

The intended outcome is a recreation that (a) captures everything proven (3-tier cognitive stack, JSONL log shape, idempotent 9-phase bootstrap, dual-key SSH story, Tirith path-pinning with `fail_open: false`, gstack-required team mode, SKILL.md surfacing of commands), (b) collapses the accreted patterns into a small, shared, typed runtime, and (c) wraps the engine with the customer-facing product surface — public API, identity, billing, dashboard, status, compliance — required to actually sell this service. The recreation will land in a new repo (`owera-fleet`) plus a new sibling repo (`owera-cloud` for the customer-plane web stack), with the current repo archived intact under `docs/archive/` for traceability.

## Product framing & business model

**Company & jurisdiction.** Owera Software Ltda is a **Brazilian company** headquartered in **Macapá, Amapá** (north Brazil, UTC−3, no DST). The legal seller / merchant-of-record is the Brazilian entity. This shapes several plan decisions:
- **Compliance**: Brazilian **LGPD** (Lei Geral de Proteção de Dados, 13.709/2018) is mandatory from day one — it applies to any data on Brazilian residents regardless of where it's processed. **GDPR** triggered once we onboard an EU customer. **SOC 2** trajectory targets US enterprise demand (12-month horizon).
- **Tax**: Brazilian SaaS exports carry ISS (municipal) + possibly PIS/COFINS depending on classification. Pricing math must include effective-tax-rate net of customer-paid VAT. Engagement with a Brazilian tax accountant is a precondition to billing real money.
- **Currency**: Start with **USD as primary** (international customers, simpler Stripe setup); add **BRL pricing** as Brazilian customers come online. Stripe Connect handles multi-currency; Brazilian merchants commonly use Stripe Brazil (boleto + Pix optional later).
- **Time zone**: SLA windows and on-call schedules expressed in UTC; customer-facing documentation acknowledges Brazil business hours (08:00–18:00 BRT = 11:00–21:00 UTC) for human support.
- **Data residency**: The gateway and worker Macs are physically in Macapá. EU customers will ask about data residency — the answer is "data is processed in Brazil; we offer DPA terms; SCCs apply for EU↔BR transfer." If this becomes a deal-blocker, evaluate a cloud-Mac sidecar (MacStadium / AWS EC2 Mac) in EU region — defer until demand surfaces.
- **Language**: Customer-facing surface starts **English-only** for global reach. Add Portuguese (pt-BR) for the dashboard and docs as a V2+ task once Brazilian customer count justifies localization cost.

**Brand & domains.** The product is **Owera Agentic** (the customer-facing name for the agentic-work service). `owera.com` is the corporate / marketing domain (out of scope for this plan); `owera.ai` is the product surface — `api.owera.ai` (public API), `app.owera.ai` (customer dashboard), `status.owera.ai` (public status). Email: `hello@owera.com` for sales/support, `noreply@owera.ai` for product transactional mail. Internal repos retain unbranded names (`owera-fleet`, `owera-cloud`); the product name "Owera Agentic" appears in the dashboard chrome, marketing copy, OpenAPI title, and email templates.

**What we're selling.** Agentic work as a managed service, packaged as a catalog of SKUs grouped into five categories. Four anchor SKUs are derived directly from v2 §3 use cases; eight more extend the catalog to cover the natural agentic surface the fleet enables (long-running cron, multi-node fan-out, native Apple toolchain, web/browser research, ledger-backed billing). The catalog is **declarative and extensible** — each SKU is one PR against `internal/catalog` (no core changes); the rollout sequence below ramps from 2 SKUs at MVP to 12 over the first year.

**Software Engineering**

| SKU | Description | Pricing | SLA | Margin lever |
|-----|-------------|---------|-----|--------------|
| **app-build** *(v2 §3.1)* | Custom iOS/macOS SwiftUI app generation with PR delivery | Per-build fixed + overage on build minutes | ≤24 h request → PR | Reuse boilerplates; cap LLM tokens per phase |
| **xcode-ci** | Hosted Xcode CI on the Mac fleet for teams without their own Mac CI | Per-build (per-minute tier) | <15 min queue | Mac fleet is the unique advantage; no x86 emulation |
| **code-audit** | Recurring code-quality + security audit on a target repo, opens issues + PRs nightly | Monthly subscription per repo + overage on findings | Daily run, ≤2 h completion | Cached AST + diff-only analysis between runs |
| **dep-upgrade** | Continuous dependency upgrade service — bumps deps, runs tests, ships only green PRs | Monthly subscription per repo + per-PR fee | Weekly cadence | gh + test-runner reuse; idempotent retries |
| **test-author** | Generate + maintain E2E test suites; iterate on flake rate as the codebase evolves | Per-suite onboarding + monthly maintenance | First suite ≤7 days | Reuse existing harness patterns; flake metrics in ledger |
| **migration-pilot** | Staged migration (Python 2→3, Rails 5→7, monolith→microservices) with proposed + executed + verified steps | Fixed project fee + per-PR delivery | Project-scope estimate | Long-running, high-value; ledger replay is the killer feature |
| **incident-postmortem** | Ingest logs + traces + ticket history; draft a post-mortem with timeline + RCA | Per-incident fixed | ≤24 h after incident close | Web + Hermes research; one-shot revenue |

**Marketing & Content**

| SKU | Description | Pricing | SLA | Margin lever |
|-----|-------------|---------|-----|--------------|
| **campaign-swarm** *(v2 §3.2)* | Coordinated multi-channel launch (Twitter/LinkedIn/email) | Per-campaign tiered (S/M/L) by # channels + posts | ≤4–12 h | Worker fan-out; cached creative assets |
| **content-batch** | Batch blog / article / social-content generation from a brief, with edit cycles | Per-piece + monthly retainer tiers | Per-piece ≤24 h | High LLM volume → parallel fan-out across nodes |
| **docs-author** | Generate + maintain technical documentation for a code repo; refresh on each merge | Monthly subscription per repo | Docs current within 24 h of merge | Diff-only doc regeneration; reuse code-audit ASTs |

**Customer Operations**

| SKU | Description | Pricing | SLA | Margin lever |
|-----|-------------|---------|-----|--------------|
| **triage-watch** *(v2 §3.3)* | Continuous Zendesk/helpdesk triage with escalation to client pager | Monthly subscription + overage per ticket above tier | <2 min ticket response | Long-running cronjob; batch LLM calls |
| **inbox-triage** | Email inbox triage (Gmail/Outlook) — categorize, draft replies, route | Monthly subscription per inbox | <5 min response time | Reuses triage-watch core; broader ICP |
| **lead-enrich** | CRM lead enrichment — gather web context, score, populate fields for a lead list | Per-1000-leads bulk + monthly retainer | Bulk ≤6 h | Web/browser toolsets + parallel fan-out |

**Research & Intelligence**

| SKU | Description | Pricing | SLA | Margin lever |
|-----|-------------|---------|-----|--------------|
| **research-brief** | Deep web research with citations on a topic (competitive intel, market sizing, due diligence) | Per-brief fixed (S/M/L by depth) | ≤24 h delivery | Hermes web/browser + structured report rendering |
| **monitor-watch** | Continuous monitoring of a topic / competitor / asset — daily digest with deltas | Monthly subscription per watch | Daily digest by 9 a.m. local | Cronjob + diff-since-last-run; complementary to research-brief |

**Data & Pipelines**

| SKU | Description | Pricing | SLA | Margin lever |
|-----|-------------|---------|-----|--------------|
| **etl-flow** *(v2 §3.4)* | Nightly PostgreSQL → Spark → Snowflake pipeline as a service | Monthly subscription + per-GB processed | Nightly window, 99.9 % success | Parallel fan-out; marker-based resume |

### SKU rollout sequence

The catalog ramps incrementally. Each tier is gated by **operator-plane primitives ready** + **PM signal of demand from customer-discovery**.

| Tier | When | Active SKUs | Cumulative |
|------|------|-------------|------------|
| **V0 (private beta)** | End of Wave 8 (Phase 3 complete) | `triage-watch`, `campaign-swarm` | 2 |
| **V1 (public GA)** | End of Wave 10 (GA gate) | + `research-brief`, `code-audit` | 4 |
| **V2 (90 days post-GA)** | After first 3 paying customers | + `dep-upgrade`, `inbox-triage`, `monitor-watch`, `content-batch` | 8 |
| **V3 (180 days post-GA)** | After SOC 2 readiness + Apple-toolchain hardening | + `xcode-ci`, `app-build`, `docs-author`, `incident-postmortem` | 12 |
| **V4 (12 months+)** | Demand-driven | `test-author`, `migration-pilot`, `lead-enrich`, `etl-flow` | up to 16 |

`etl-flow` lands later than its v2 §3 origin suggests because it requires upstream/downstream credential management at customer scale — easier after the platform proves out with simpler SKUs.

### Why these SKUs are essential (selection rationale)

- **Pick SKUs that exercise the fleet's unique strengths**, not generic LLM wrappers anyone can build:
  - Long-running cronjob + signed ledger → `triage-watch`, `inbox-triage`, `monitor-watch`, `dep-upgrade`, `code-audit`, `etl-flow`
  - Native Apple toolchain on Mac fleet → `xcode-ci`, `app-build`, `macos-build`
  - Multi-node fan-out → `campaign-swarm`, `content-batch`, `lead-enrich`
  - Web/browser research → `research-brief`, `monitor-watch`, `incident-postmortem`, `lead-enrich`
- **Diversify revenue shape**: mix of one-shot (`app-build`, `research-brief`, `migration-pilot`, `incident-postmortem`) and recurring (`triage-watch`, `monitor-watch`, `code-audit`, `dep-upgrade`, `etl-flow`). Recurring drives MRR; one-shot funds growth and broadens the ICP.
- **Each SKU is one PR**: `internal/catalog/<sku>.go` declares `{Name, Version, InputsSchema, Pricing, SLA, Dispatcher}`. The dispatcher composes existing operator-plane primitives (`delegate`, `swarm`, `cronjob`, `swarm + markers`, `research`) — new SKUs land without core changes.
- **Out of scope** for now: anything requiring sustained x86 native (most non-Apple commercial software builds), heavy GPU (training), regulated healthcare/finance data flows (HIPAA/PCI) until compliance ramps up. These belong in V4+ once SOC 2 Type 2 is achieved.

**Who consumes it.** External developer-shop / SaaS clients with budget for AI-assisted ops work but no internal infra to run multi-node Hermes fleets. Initial GTM via Owera's existing network; private beta first.

**Two planes, one product.**

- **Operator plane** (the fleet — Phases 1–2 of this plan): Hermes gateway + worker Macs, JSONL logs, ledger, swarm orchestrator, audit, scenarios. Operated by Rodrigo (and Claude). *Not* exposed to customers directly.
- **Customer plane** (Phase 3): public HTTPS API, identity, service catalog, job queue, billing, dashboard, status page. Cloud-hosted edge ingress that tunnels into the gateway over a private link. *This* is what customers see.

The plumbing already specified for v2 §4 (signed ledger, DM pairing, per-pairing rate limit, signed billing event, client-id) is precisely the foundation the customer plane consumes — `pairing` becomes "customer", `signed ledger event` becomes "Stripe usage record", `per-pairing budget` becomes "monthly cost cap on the account". v2 §4 is the seam where Phases 1-2 meet Phase 3.

**Phasing toward a sellable product.**

- **Phase 1 — Foundation** (operator-plane Go core; replaces today's 23 bash scripts).
- **Phase 2 — Use-case primitives** (the v2 §3+§4 primitives the SKUs depend on: ledger, pairing, secrets, swarm, markers, alerting, metrics).
- **Phase 3 — Productization** (customer plane: API gateway, identity, catalog, job queue, billing, dashboard, status, compliance baseline). *This is the phase that turns the engine into a product.*
- **Phase 4 — Launch readiness** (private beta with 1–3 design partners, runbooks, on-call rotation, GA gates).

**MVP scope (private beta cut).** Start with **2 SKUs (V0)**: `triage-watch` (recurring revenue, narrow surface, well-defined SLA) and `campaign-swarm` (high perceived value, finite duration). Ramp to **4 SKUs by GA (V1)** adding `research-brief` (one-shot revenue, broadens ICP) and `code-audit` (recurring, B2B-friendly). Beyond GA, the catalog grows in tiers V2–V4 above; each SKU is one PR-sized contribution against `internal/catalog`.

**Non-goals for this plan.** Marketing site, lead funnel, paid ads, sales hiring — outside the engineering scope here. Open question 6 (below) carves out where product/growth work belongs.

## Architecture principles (bottom-up)

### Operator-plane principles (Phases 1–2)

1. **One dispatcher, many subcommands.** `fleetctl` (Go single binary) replaces 23 separate scripts. Discoverable, tab-completable, consistent flags, structured errors.
2. **Typed core, shell at the edges.** Go owns argument parsing, config loading, node enumeration, SSH, JSONL logging, report rendering, audits, LaunchAgent lifecycle, scenario running. Bash is reserved for *remote-executed* phase fragments scp'd to workers.
3. **Generated docs over hand-edited.** `STATE.md` is `fleetctl state --markdown`. Security drift table is `fleetctl audit config`. Operators hand-edit only *stable* docs.
4. **Bootstrap is a single command, zero manual steps.** `fleetctl bootstrap-worker hermes@new.local` covers brew + Hermes + key handoff + secrets + LaunchAgents loaded.
5. **LaunchAgents installed = loaded.** No installed-but-disabled drift.
6. **Tests are data, not bash.** Scenario YAMLs; one runner (`fleetctl test --tier=...`).
7. **SKILL.md generated from command metadata.** No parallel maintenance of two files.
8. **gstack stays required.** PreToolUse hook unchanged.
9. **Bash 3.2 compat remains, for remote fragments only.** ~10 files; everything else escapes the constraint.
10. **Nodes registry stays simple.** `~/.hermes/nodes.txt` is enough — appropriate for actual fleet size.

### Customer-plane principles (Phase 3)

11. **Customers never touch the gateway directly.** A cloud-hosted edge service (Cloudflare Tunnel + a Go API in Fly.io / Render / Cloud Run) is the only public surface. The gateway Mac mini is a private control plane behind a Tailscale / Cloudflare-tunnel ingress.
12. **Multi-tenancy from day one.** Every Phase-3 data model has `tenant_id` as a first-class column. Even with one customer in private beta, no shared state across tenants.
13. **Identity and pairing are the same concept.** The Phase-2 `internal/pairing` model is promoted: pairings *are* customers (or customer-API-keys); revocation, rate limit, budget all attach to the tenant.
14. **Billing reads the ledger; the ledger is the source of truth.** Stripe usage records are emitted *from* signed `internal/ledger` events, never directly from delegate calls. If the ledger and Stripe disagree, the ledger wins and Stripe is reconciled.
15. **Job lifecycle is a state machine, exposed to the customer.** Submitted → queued → running → succeeded / failed / cancelled. Customer dashboard renders the same state machine the operator plane stores.
16. **API surface is small and stable; SKUs are versioned.** `POST /v1/jobs` with `sku` + `inputs`; `GET /v1/jobs/{id}`; `POST /v1/jobs/{id}/cancel`. Each SKU declares its inputs schema. SKU schemas are versioned (`triage-watch@v1`); breaking changes ship as `@v2`.
17. **Audit log captures every customer-affecting action.** Tied to compliance trajectory (SOC 2 Type 1 within 12 months of GA).
18. **Status page is public.** `status.owera.ai` reflects fleet health + SLA breach incidents. Driven by `internal/metrics`.
19. **Operator and customer planes never share a single failure domain.** If the gateway Mac mini falls off the network, the edge API returns a clean 503 with retry-after, not a hang. If the edge falls over, the operator plane keeps processing the queue and the dashboard catches up on recovery.
20. **Cost-per-task is a first-class metric.** `internal/metrics` exposes per-SKU + per-tenant unit-cost gauges. Pricing decisions are data-driven from day one.
21. **Public-repo discipline.** Both `owera-fleet` and `owera-cloud` are public from day one. Implications:
    - **Zero secrets in git, ever.** No `.env`, no Stripe keys, no Cloudflare tokens, no minisign private keys, no customer identifiers. Pre-commit hook (`gitleaks` or `trufflehog`) on both repos; CI re-runs the scan as a gate.
    - **Customer data never appears in the repo or in CI logs**, even in test fixtures. Use synthetic fixtures keyed to `tenant_id: "fixture-<n>"`.
    - **The hermes-setup archive under `docs/archive/` must be scrubbed before publication.** The current `hermes-setup` is already on GitHub at `rrecio/hermes-setup`; the archive simply embeds it — verify no unsigned secrets snuck in during recent commits.
    - **Public discoverability is a feature**: README, status page, OpenAPI spec, and SKU docs become marketing surface. Tone in public docs is customer-friendly without being marketing fluff.
    - **External contributors are possible but not encouraged.** CONTRIBUTING.md says "issues welcome, PRs at maintainer discretion." Code of conduct standard.
    - **Compliance / security posture is visible.** `compliance/policies/*` documents are published; this is a trust signal for prospective customers, not a leak. Sensitive *implementations* (e.g., specific PagerDuty schedules, customer IDs) live outside git — in 1Password, in cloud secret managers.

## Target directory layout — two repos

### Repo 1: `owera-fleet` (operator plane; Go monorepo)

```
owera-fleet/
├── README.md                       repo entry; quickstart + common ops
├── CLAUDE.md                       conventions; gstack requirement; reading order
├── LICENSE                         IP terms (carried from current repo)
├── go.mod / go.sum                 Go 1.22+ module
├── cmd/
│   └── fleetctl/main.go            single binary entry; cobra-style subcommands
├── internal/                       Go packages (typed core)
│   ├── log/                        JSONL logger; schema {ts,node,phase,action,result,duration_ms,stderr_tail}
│   ├── ssh/                        SSH client + sudo-via-expect-fallback for one-shot password handoff
│   ├── nodes/                      ~/.hermes/nodes.txt parser; iteration helpers
│   ├── report/                     markdown report builder; consistent headers/footers
│   ├── audit/                      config-drift, dotfile-drift, process-inventory
│   ├── launchd/                    plist renderer + bootstrap/bootout
│   ├── bootstrap/                  10 phases (now includes brew baseline as Phase 0)
│   ├── hermes/                     thin wrapper over `hermes` CLI invocations + version pinning
│   ├── scenarios/                  YAML loader + test runner (tiered)
│   ├── secrets/                    .env.gpg handling; never logs cleartext
│   └── state/                      live snapshot collector for `fleetctl state`
├── cmd/fleetctl/commands/          one file per subcommand; thin orchestration
│   ├── bootstrap.go                bootstrap a worker (replaces bootstrap-hermes-node.sh end-to-end, incl. brew)
│   ├── delegate.go                 run cmd on one/all/random worker
│   ├── review.go                   three-tier code review (worker analysis → Hermes draft)
│   ├── research.go                 web-research delegation via `hermes -z`
│   ├── health.go                   snapshot + diff (one command, --diff flag)
│   ├── audit.go                    config + dotfile + process audits (subcommands)
│   ├── update.go                   coordinated version bump (gateway + workers)
│   ├── smoke.go                    minimal worker reachability probe
│   ├── test.go                     scenario runner; --tier flag
│   ├── state.go                    regenerate STATE.md-equivalent on demand
│   ├── setup_agent.go              install + load any LaunchAgent (backup, watchdog, logrotate, worker-backup)
│   ├── onboard.go                  new-operator interactive walkthrough
│   └── gen_skills.go               write skills/<name>/SKILL.md from command metadata
├── remote/                         bash fragments scp'd to workers (bash 3.2)
│   ├── phase00_brew_baseline.sh
│   ├── phase02_create_user.sh
│   ├── phase03_install_hermes.sh
│   ├── phase07_install_tirith.sh
│   ├── heartbeat_writer.sh
│   ├── log_rotator.sh
│   └── with_pw.exp                 one-time sudo password handoff
├── templates/
│   ├── launchd/                    5 plists with Go-template substitutions
│   │   ├── heartbeat.plist.tmpl
│   │   ├── backup.plist.tmpl
│   │   ├── backup-worker.plist.tmpl
│   │   ├── watchdog.plist.tmpl
│   │   └── logrotate.plist.tmpl
│   └── reports/                    markdown skeletons (Go text/template)
├── scenarios/
│   ├── smoke.yaml                  reachability + binary-presence probes
│   ├── usecase.yaml                operator workflows (delegate, review, research, observe, lifecycle, resilience)
│   ├── readiness.yaml              v2 §3 aspirational primitives (PASS/GAP/FAIL with GAP → roadmap)
│   └── e2e.yaml                    end-to-end demos (custom-agent, marketing fan-out, ticket triage, ETL, native-app build)
├── skills/                         generated by `fleetctl gen-skills` (committed; CI verifies in sync)
│   ├── delegate/SKILL.md
│   ├── review/SKILL.md
│   ├── research/SKILL.md
│   ├── health/SKILL.md
│   ├── audit/SKILL.md
│   ├── test/SKILL.md
│   └── ... (one per surface)
├── docs/
│   ├── operation.md                stable architecture (was OPERATION.md)
│   ├── roadmap.md                  forward view (was ROADMAP.md)
│   ├── plan.md                     unified deployment plan (v2 + review collapsed; §0 "What v1 got wrong")
│   ├── security.md                 ground-truth config; live-settings + drift tables are generated, prose is hand-edited
│   └── archive/
│       ├── README.md               warning + pointer to ../plan.md
│       ├── hermes-setup-2026-05-16/  full snapshot of current repo (preserved for traceability)
│       ├── plan-v1.txt
│       ├── plan-v1-review.md
│       ├── hermes-services-guide.txt
│       ├── data-prevention-guide.md
│       └── important-notes.md
├── .claude/
│   ├── settings.json               PreToolUse hooks (gstack-check, shellcheck for remote/)
│   └── hooks/
│       ├── check-gstack.sh
│       └── shellcheck-remote.sh
├── .github/workflows/
│   ├── ci.yml                      Go test, golangci-lint, shellcheck on remote/, skill-manifest drift check
│   └── release.yml                 cross-compile darwin/arm64 + darwin/amd64 binaries; attach to release
└── test/                           Go unit + integration tests
    ├── log_test.go
    ├── ssh_test.go
    ├── nodes_test.go
    ├── audit_test.go
    ├── scenarios_test.go
    └── fixtures/                   sample config.yaml, nodes.txt, JSONL inputs
```

### Repo 2: `owera-cloud` (customer plane; Phase 3+)

```
owera-cloud/
├── README.md
├── CLAUDE.md
├── LICENSE
├── api/                            Go HTTP service: public API gateway
│   ├── cmd/apiserver/main.go       binds :8080; deployed to Fly.io / Cloud Run
│   ├── internal/
│   │   ├── auth/                   API-key + OAuth (Clerk or WorkOS) auth middleware
│   │   ├── identity/               tenants, users, API keys; multi-tenant data model
│   │   ├── catalog/                SKU registry; per-SKU schema + pricing + SLA
│   │   ├── jobs/                   job lifecycle state machine; status, cancel
│   │   ├── queue/                  durable queue (start with SQLite/litestream, evolve to Redis/PG)
│   │   ├── dispatcher/             translates customer job → fleetctl operator-plane invocation
│   │   ├── billing/                Stripe meters; usage records from ledger; invoice events
│   │   ├── audit/                  compliance audit log (every customer-affecting action)
│   │   ├── status/                 fleet/SLA health publisher; drives the public status page
│   │   └── webhooks/               Stripe + customer outbound webhooks
│   └── openapi.yaml                versioned API spec; clients generated from it
├── tunnel/                         Cloudflare Tunnel / Tailscale config to reach the gateway
├── web/                            Next.js customer dashboard
│   ├── app/                        App Router routes: /dashboard, /jobs, /billing, /api-keys, /support
│   ├── components/                 shared UI; design system
│   ├── lib/                        API client (generated from openapi.yaml)
│   ├── public/
│   └── package.json
├── status/                         public status page (Next.js or static-generated)
├── infra/                          IaC: Fly.io / Cloud Run / Cloudflare config + secrets manifest (gitops, but no secrets)
│   ├── api.fly.toml
│   ├── web.vercel.json
│   ├── tunnel.cloudflare.yaml
│   └── README.md                   how to deploy + rotate secrets
├── compliance/                     SOC 2 trajectory artifacts
│   ├── policies/                   security, access, incident response, data retention
│   ├── runbooks/                   incident response, on-call playbooks
│   ├── audit-controls/             mapping controls → evidence
│   └── README.md
├── docs/
│   ├── api.md                      developer-facing API reference (generated from openapi.yaml)
│   ├── pricing.md                  SKU pricing + tiers + caps
│   ├── support.md                  customer support runbook
│   └── onboarding.md               new-customer ramp guide
├── .github/workflows/              CI: Go test, web build, openapi lint, deploy gates
└── tests/                          integration: API ↔ operator-plane round-trip
```

Why a second repo: the operator plane is a CLI that ships as static binaries to operators; the customer plane is a long-running HTTP service + web app with very different CI, dependency surface, and release cadence. Mixing them would force one repo's tooling on the other. The two repos talk through a stable contract (the operator plane exposes a minimal `fleetctl serve` JSON-RPC endpoint over the Cloudflare tunnel; the cloud's dispatcher calls it).

## Component-by-component design

### `internal/log` — single JSONL logger

Preserve the proven schema `{ts, node, phase, action, result, duration_ms, stderr_tail}`. Implement once in Go; expose `log.Action(ctx, node, phase, action, result, dur, stderrTail)`. All commands route through it. The bash fragments in `remote/` use a minimal inline equivalent and stream their JSONL back over stderr for the gateway to fold into the local log stream.

### `internal/ssh` — SSH with one-shot sudo handoff

Use `golang.org/x/crypto/ssh` for the steady-state (post-Phase 6) dedicated-key path. For the *initial* operator-key + sudo password handoff (Phase 2: create-user), shell out to `remote/with_pw.exp` exactly once with the password passed via a private fd, never via env or argv. After Phase 6 the dedicated key is in the macOS Keychain and `with_pw.exp` is never used again. Drops the brittle env-var-across-Bash-tool-boundary problem we hit today with `install-brew-on-worker.sh`.

### `internal/bootstrap` — 10 phases, idempotent

The proven 9 phases stay, with a new **Phase 0: brew baseline** that runs `remote/phase00_brew_baseline.sh` to install Homebrew + essentials (`jq coreutils shellcheck gh wget ripgrep`) on a fresh worker. This eliminates the untracked `install-brew-on-worker.sh` flow entirely. Each phase reports `applied`/`no-change`/`failed`+remediation. Re-running is safe.

### `internal/audit` — drift detection as code

Three sub-audits, each callable independently and as a bundle:

- **Config audit**: parse `~/.hermes/config.yaml` (gateway and each worker), compare against a baseline in `docs/security.md` frontmatter, emit a drift table identical in shape to today's SECURITY_NOTES.md drift table — but generated.
- **Dotfile audit**: hash shell config files (`.zshrc`, `.bashrc`, `.profile`, `.ssh/config`) across the fleet, surface divergence. Replaces `fleet-dotfile-check.sh`.
- **Process audit**: long-running processes, listening ports, `launchctl list` entries. Replaces `fleet-process-inventory.sh`.

### `internal/launchd` — install = loaded

Render plist from template, write to `~/Library/LaunchAgents/<name>.plist` or `/Library/LaunchDaemons/<name>.plist`, `launchctl bootstrap gui/$UID <plist>`, then verify `launchctl print` returns expected state. Failure mode is loud and remediated. One package replaces all 5 install-*.sh installers.

### `cmd/fleetctl/commands/test.go` + `scenarios/*.yaml`

Three test scripts collapse to one runner reading declarative scenarios. Each scenario YAML declares: `id`, `tier`, `description`, `setup`, `steps` (with assertions), `teardown`, `expected_artifacts`. Tiers map directly to today's layers but encoded:
- `smoke` ← `smoke-test-node.sh` (reachability)
- `usecase` ← `fleet-usecase-tests.sh` (operator workflows)
- `readiness` ← `fleet-readiness-probes.sh` (primitives with PASS/GAP/FAIL)
- `e2e` ← `fleet-usecase-demos.sh` (end-to-end demos)

GAPs from readiness tier auto-write a roadmap entry stub to `docs/roadmap.md` (under a `<!-- generated -->` block).

### `cmd/fleetctl/commands/state.go` — STATE.md replacement

Reads live: `~/.hermes/PINNED_VERSION`, `~/.hermes/LAST_BACKUP`, `~/.hermes/config.yaml`, `launchctl print` for each Hermes agent, `nodes.txt`, recent JSONL summary. Renders a markdown report (or JSON with `--json`). Today's STATE.md becomes a thin wrapper: a header + the command output appended below a `<!-- generated, do not hand-edit -->` marker.

### `cmd/fleetctl/commands/gen_skills.go` — SKILL.md generation

Each command in `cmd/fleetctl/commands/` declares a `Skill()` function returning `{Name, Trigger, Summary, Args, Examples}`. `fleetctl gen-skills` writes `skills/<name>/SKILL.md`. CI checks the on-disk skills match generated output (drift = fail). Eliminates the parallel-maintenance of two files.

## Critical files to write / port (priority order)

| Order | Component | Source today | Notes |
|-------|-----------|--------------|-------|
| 1 | `internal/log`, `internal/json`, `internal/nodes`, `internal/report` | inline patterns across all `scripts/*.sh` | foundational; unit-tested first |
| 2 | `cmd/fleetctl/main.go` + `commands/state.go` | n/a | proves the dispatcher pattern with a trivial read-only command |
| 3 | `internal/ssh` + `remote/with_pw.exp` | `scripts/lib/ssh.sh`, `scripts/lib/with_pw.exp` | dual-mode (initial / dedicated key) |
| 4 | `internal/bootstrap` + `remote/phase*.sh` | `scripts/bootstrap-hermes-node.sh`, `scripts/lib/phases.sh`, untracked `install-brew-on-worker.sh` | adds Phase 0 brew; collapses untracked step |
| 5 | `commands/delegate.go`, `commands/health.go`, `commands/smoke.go` | `scripts/delegate-to-node.sh`, `fleet-health-{snapshot,diff}.sh`, `smoke-test-node.sh` | daily-use surface; validates lib/ssh + lib/report |
| 6 | `internal/audit` + `commands/audit.go` | `scripts/fleet-{dotfile-check,process-inventory}.sh` + hand-edited drift tables | adds first-class config drift detection |
| 7 | `internal/launchd` + `commands/setup_agent.go` | `scripts/install-{backup,backup-worker,watchdog,logrotate}.sh` | replaces 4 installers; always loaded |
| 8 | `scenarios/*.yaml` + `commands/test.go` | `fleet-{usecase-tests,readiness-probes,usecase-demos}.sh` | three scripts → one runner |
| 9 | `commands/review.go`, `commands/research.go` | `scripts/review-branch.sh`, `research-with-hermes.sh` | Hermes-delegation commands |
| 10 | `commands/gen_skills.go` + `skills/*` | hand-written `scripts/skills/agentic-services/*/SKILL.md` | auto-generated from command metadata |
| 11 | `commands/onboard.go`, `commands/update.go` | `scripts/update-owera-fleet.sh` + new onboarding flow | operator UX polish |
| 12 | `docs/{operation,roadmap,plan,security}.md` + `archive/` | current root docs | unified plan from v2+review; archive v1, v1-review, services-guide, data-prevention, important-notes |
| 13 | `.github/workflows/{ci,release}.yml` | n/a | Go test + lint + shellcheck + skill-drift + cross-compiled binaries |

## Migration approach (new repo, not refactor)

Per your selection, the recreation lives in a **new repo** (`owera-fleet`), not as edits to the current `hermes-setup` workspace. Step-by-step:

1. `git init owera-fleet` next to `hermes-setup`. Carry over `LICENSE`, `CLAUDE.md` (revised), and `.claude/settings.json` first so gstack enforcement is in place from commit 1.
2. **Snapshot the current repo into `docs/archive/hermes-setup-2026-05-16/`** as a complete preserved tree. This is the only place historical scripts and docs live; the rest of the new repo starts empty.
3. Build the components in priority order above. Each row is one PR-sized chunk.
4. Live coexistence: until `fleetctl bootstrap-worker` is proven end-to-end against `claw1.local`/`claw2.local`, the old `hermes-setup` repo remains the operational source of truth. Cut over only after the new `fleetctl state` agrees byte-for-byte with the old `STATE.md` and `fleetctl bootstrap-worker` succeeds on a clean test node.
5. After cutover, the old repo's `README.md` gets a single line: "Superseded by [owera-fleet](../owera-fleet); preserved for history."

## Verification (how to test the recreation end-to-end)

1. **Unit tests**: `go test ./...` over `internal/` packages. Targets ≥80% coverage on `log`, `ssh`, `nodes`, `audit`, `scenarios`.
2. **Shellcheck**: every file in `remote/` passes `shellcheck -s bash` clean.
3. **Skill manifest drift**: `fleetctl gen-skills --check` fails CI if `skills/` diverges from generated output.
4. **State-equivalence test**: `fleetctl state --markdown > /tmp/new-state.md`; diff against the hand-maintained `STATE.md` from the old repo at the same moment. Tolerable diffs only (timestamps, JSONL tail sampling).
5. **Bootstrap-test on a clean node**: provision a fresh macOS VM, run `fleetctl bootstrap-worker hermes@vm.local`, expect 10 phases to land idempotently and heartbeat to be < 2 min old at exit. Re-run the same command — every phase reports `no-change`.
6. **Scenario tiers run green**: `fleetctl test --tier=smoke`, then `--tier=usecase`, then `--tier=readiness`, then `--tier=e2e` against the live fleet. Match or exceed today's pass-rate (7 PASS / 1 WARN / 1 FAIL / 1 SKIP from the May 16 Layer-1 run; 4 PASS / 1 WARN from Layer-3).
7. **Drift audit**: `fleetctl audit config` against `claw1` + `claw2` reproduces today's SECURITY_NOTES.md drift table.
8. **Backup round-trip**: `fleetctl setup-agent backup --install-and-load`, wait for the next scheduled run, then `restic snapshots` confirms a new snapshot. Restore a single file to /tmp and diff against the live source.
9. **Update flow**: `fleetctl update --target v0.14.0 --dry-run` then `--apply` against a test node. Rollback via `fleetctl update --rollback` returns to v0.13.0 cleanly.

**Phase 3+4 (customer-plane) verification:**

10. **End-to-end paid job per SKU**: For *each* active SKU in the catalog tier under verification, run: fresh tenant → API key → `POST /v1/jobs` with that SKU → operator plane runs → ledger emits a `bill` event → Stripe usage record → daily reconciliation shows zero drift. At V0 (private beta): 2 SKU end-to-end runs. At V1 (GA): 4 SKU end-to-end runs. Etc. Test in Stripe test mode first; repeat in live mode before any tier ships.
11. **Cross-tenant isolation**: Adversarial CI test — tenant A authenticates and requests tenant B's job by ID. Assert 404 (not 403; existence is itself sensitive). Audit log shows the attempt; no data leaks.
12. **API contract stability**: `openapi.yaml` diff between `main` and PR is checked; breaking changes blocked unless the SKU/endpoint is bumped to `@v2`.
13. **Status page accuracy**: Synthetic incident — disable the gateway tunnel for 60 s. Status page reflects degradation within 60 s of detection; clears within 60 s of recovery.
14. **Billing accuracy**: Inject 1,000 synthetic ledger `bill` events with known per-tenant totals. Verify Stripe-side usage equals the input within 0.5 % across all tenants. Reconciliation job logs the drift figure.
15. **On-call drill** (T19.6): Operator on pager. Simulate gateway outage. PagerDuty fires within 2 min. Runbook followed end-to-end. Post-mortem authored within 48 h.
16. **Customer onboarding flow**: From a fresh email, complete signup → email-verify → API key issue → first job → see result in dashboard → see invoice. Time-to-first-success <30 min for the design partner.
17. **Compliance evidence inventory**: For each SOC 2 Common Criteria, the mapping in `compliance/audit-controls/` resolves to a real evidence file (signed ledger sample, audit log excerpt, policy doc, runbook). External auditor's pre-assessment passes the readiness check.

## v2 §3+§4 use-case coverage — what Phase 1 delivers, what's still needed

The component-by-component design above is **Phase 1: foundation**. It delivers the dispatcher, bootstrap, audit, scenario runner, SSH, JSONL log, LaunchAgents — i.e., everything needed to *host* the v2 use cases. It does **not** yet deliver the use-case-specific primitives that v2 §3 (Custom iOS app / Marketing swarm / Ticket triage / ETL) and v2 §4 (Operational Practices) demand. Phase 2 below closes that gap.

### Coverage matrix vs v2 §3 + §4

| Requirement (v2 reference) | Phase 1 status | Closed by Phase 2 component |
|---|---|---|
| Signed per-task ledger `~/.hermes/ledger/<task-id>.jsonl` for replay/compensation (§3.1, 3.2, 3.4) | not covered | `internal/ledger` |
| DM pairing + per-pairing rate limit + cost cap (§3.1, §4) | not covered | `internal/pairing`, `internal/budget` |
| `run-with-secrets` stdin-fed wrapper for use-case-time secret delivery (§3.2, 3.3) | bootstrap installs the script (Phase 5) but it's not surfaced as a use-case primitive | `internal/secrets` + `fleetctl run-with-secrets` |
| Signed config sync gateway → nodes, minisign / pinned pubkey verification (§4) | not covered (audit only, read-only) | `internal/configsync` + `fleetctl config-sync` |
| Orchestrator + leaf-subagent swarm with per-worker ledger merge (§3.2) | `delegate` is one-shot only | `internal/orchestrator` + `fleetctl swarm` |
| `hermes cronjob` wrapper (NOT macOS cron) for ticket-triage poller (§3.3) | launchd only | `internal/hermesjobs` + `fleetctl cronjob` |
| Marker-file primitive: content-hash + freshness check for ETL (§3.4) | not covered | `internal/etl` (or `internal/markers`) |
| Pager-first alerting (PagerDuty/OpsGenie), chat secondary (§3.3, 3.4, §4) | watchdog still goes to ntfy.sh | `internal/alerting` (multi-backend: PagerDuty/OpsGenie/ntfy) |
| Prometheus/Datadog metrics: heartbeat age, request rate, per-task p50/p95 latency, delegate_task error rate (§4) | not covered | `internal/metrics` + `fleetctl metrics serve` |
| Signed skill commits + scheduled pull-and-verify on nodes (§4) | auto-gen SKILL.md only, no signing | `internal/skillsync` + `fleetctl skill-sync` (extends `gen-skills`) |
| Per-pairing budget store keyed to the signed ledger (§4) | not covered | `internal/budget` (shares store with `internal/ledger`) |
| Nightly cross-node log rsync to gateway (§4) | only per-node rotation | `internal/logaggregate` + LaunchAgent template |

### Phase 2: use-case primitives (extends Phase 1 plan above)

Adds to `internal/`:

```
internal/
├── ledger/            signed JSONL ledger per task-id; sign with minisign; verify on read; replay/compensate API
├── pairing/           DM pairing tokens + per-pairing identity; tied to client-id signature
├── budget/            per-pairing rate limit + CPU-seconds/day + cost cap; reads/writes ledger
├── secrets/           run-with-secrets wrapper API; passes plaintext via stdin to subprocess; never argv/env/disk
├── configsync/        minisign-signed config bundles; push gateway → nodes; node-side verify against pinned pubkey
├── orchestrator/      parent-agent + N leaf-subagent swarm; merges per-worker ledgers; retry/compensation logic
├── hermesjobs/        thin wrapper over `hermes cronjob` (Hermes-managed, NOT launchd); list/install/remove
├── markers/           step.done content-hashed marker files for ETL pipelines; freshness + integrity check
├── alerting/          multi-backend: PagerDuty + OpsGenie + ntfy.sh; pager-first routing
├── metrics/           Prometheus exposition format; HTTP server on :9090; metrics from JSONL + ledger + heartbeats
├── skillsync/         signed-Git skill pulls; verify signature before applying; scheduled refresh
└── logaggregate/      nightly rsync --remove-source-files of node logs → gateway; integrates with newsyslog rotation
```

Adds to `cmd/fleetctl/commands/`:

```
commands/
├── pair.go            create/revoke DM pairings; list active; show budget
├── ledger.go          inspect ledger; verify signatures; replay a task; compensate a failed task
├── swarm.go           launch an orchestrator + N leaf subagents; merge ledgers; emit campaign report
├── cronjob.go         CRUD over hermes cronjobs (parallel to setup-agent for launchd)
├── config-sync.go     sign + push gateway config bundle to nodes; verify-only mode for audit
├── skill-sync.go      sign skill commit; trigger node-side scheduled pull; verify drift
├── metrics.go         start/stop the metrics HTTP server; one-shot scrape; expose for Prometheus
├── alert.go           fire a test alert through configured backend (sanity check pager wiring)
└── run-with-secrets.go exposed primitive: `fleetctl run-with-secrets --env-gpg .env.gpg -- <cmd...>`
```

Adds to `scenarios/`:

- `scenarios/usecase31_native_app.yaml` — exercises `pairing` → `delegate` → `ledger` → `swarm` (optional) → `gh pr create` → `alerting` on failure. Asserts: pairing rate-limit honored; ledger signed; PR URL captured; failure mid-`xcodebuild` triggers compensation path.
- `scenarios/usecase32_marketing_swarm.yaml` — exercises `swarm` with 3 leaves on Node-2, per-worker ledger merge, idempotency-key-driven compensation.
- `scenarios/usecase33_ticket_triage.yaml` — exercises `cronjob` (hermes-managed) + `secrets` (stdin) + `alerting` (pager on priority>8).
- `scenarios/usecase34_etl.yaml` — exercises `cronjob` (gateway-side) + parallel `delegate` across 3 nodes + `markers` (content-hashed) + `ledger` resume from last successful marker.

These replace today's `fleet-usecase-demos.sh` D1–D5 with scenarios that actually exercise the v2-specified primitives, not just shell out to `xcodebuild`/`curl`. The Layer-3 demos today only prove the workflow shape — Phase 2 scenarios prove the primitives.

### Phase 2 build order (after Phase 1 lands)

| Order | Component | Unblocks |
|-------|-----------|----------|
| 14 | `internal/ledger` + `commands/ledger.go` | every use case; foundational |
| 15 | `internal/secrets` + `commands/run-with-secrets.go` | use-cases 3.2, 3.3 |
| 16 | `internal/pairing` + `internal/budget` + `commands/pair.go` | use-cases 3.1, 3.2; gates traffic |
| 17 | `internal/configsync` + `commands/config-sync.go` | §4 signed config sync |
| 18 | `internal/hermesjobs` + `commands/cronjob.go` | use-cases 3.3, 3.4 |
| 19 | `internal/markers` + e2e ETL scenario | use-case 3.4 |
| 20 | `internal/orchestrator` + `commands/swarm.go` | use-case 3.2 |
| 21 | `internal/alerting` + `commands/alert.go` | §4 pager-first |
| 22 | `internal/metrics` + `commands/metrics.go` | §4 health monitoring |
| 23 | `internal/skillsync` + `commands/skill-sync.go` | §4 signed-skill pull |
| 24 | `internal/logaggregate` + new LaunchAgent template | §4 log aggregation |
| 25 | Phase-2 e2e scenarios (`usecase31`–`usecase34`) green against the live fleet | proves end-to-end delivery of v2 §3 |

After step 25, `fleetctl test --tier=e2e` passing means the v2 §3 use cases are actually deliverable — not just shaped.

### Honest characterization

- **Phase 1 alone** = a cleaner, typed, single-binary version of today's `hermes-setup`. Same surface, same primitives, same gaps. Better DX, no new capability.
- **Phase 1 + Phase 2** = the v2 plan is actually deliverable end-to-end. Today's Layer-2 readiness GAPs close one by one as Phase 2 components land. Still operator-only.
- **Phase 1 + 2 + 3** = the engine becomes a product. Customers can sign up, submit jobs via API or dashboard, get billed, and the operator plane stays out of their direct path.
- **Phase 1 + 2 + 3 + 4** = the product is sellable. Private beta with 1–3 design partners, runbooks for on-call, GA gates met.
- **The current `hermes-setup` repo today** sits roughly at "Phase 1 in bash, no Phase 2/3/4". The recreation makes the path to a sellable product an explicit sequence rather than an open-ended roadmap.

## Phase 3: Customer plane — components

### Edge & API gateway (`owera-cloud/api/`)

- **Public surface**: HTTPS, behind Cloudflare. TLS, DDoS protection, rate-limit at the edge.
- **Tunnel to operator plane**: Cloudflare Tunnel originates on the gateway Mac mini; the cloud API dials into it. No public ingress on the gateway.
- **Endpoints (v1)**: `POST /v1/jobs` (submit), `GET /v1/jobs/{id}` (status), `GET /v1/jobs` (list with tenant scoping), `POST /v1/jobs/{id}/cancel`, `GET /v1/skus` (catalog), `GET /v1/usage` (current-period meter). All authenticated by API key in `Authorization: Bearer <key>` header.
- **OpenAPI spec is the contract**: `api/openapi.yaml`. Web client + customer client SDKs both generated from it.

### Identity & multi-tenancy (`api/internal/identity/`)

- Tenants own users and API keys. Each tenant has a unique `tenant_id` injected into every job, ledger entry, audit log row, billing meter.
- Auth options: API key (primary; for programmatic), OAuth via Clerk or WorkOS (for dashboard). Start with API key only for private beta; OAuth lands before GA.
- Data isolation: row-level scoping enforced at the queue + jobs + audit layer. CI test that explicitly attempts cross-tenant access and asserts denial.

### Service catalog (`api/internal/catalog/`)

- Each SKU declared in a single Go file under `internal/catalog/<sku>.go`: `{Name, Version, Category, InputsSchema (JSON Schema), Pricing, SLA, Dispatcher, BillingMeter}`. Customer-facing.
- `Dispatcher` is the function that translates `(tenant_id, sku, inputs) → fleetctl <subcommand> --json` (or RPC). It composes existing operator-plane primitives:
  - `delegate` + `ledger` for one-shot SKUs (`app-build`, `research-brief`, `incident-postmortem`, `migration-pilot`)
  - `swarm` + `markers` for parallel SKUs (`campaign-swarm`, `content-batch`, `lead-enrich`, `etl-flow`)
  - `cronjob` (Hermes-managed) + `secrets` for watcher SKUs (`triage-watch`, `inbox-triage`, `monitor-watch`, `code-audit`, `dep-upgrade`)
  - `delegate` + native toolchain on a tagged Mac node for `xcode-ci`, `app-build`
- **Versioned**: `triage-watch@v1`. Breaking inputs change → `@v2`. Old version stays alive for a deprecation window (90 days minimum).
- **Extensibility contract**: adding a SKU = one PR with `<sku>.go` + a customer-plane scenario in `owera-cloud/tests/scenarios/<sku>.yaml` + docs update in `docs/pricing.md`. No changes to core API, queue, dispatcher, billing, or dashboard code. CI gate enforces this — a PR that touches non-catalog files when introducing a new SKU is flagged for TL review.
- **MVP catalog content** at end of Wave 8: V0 = `triage-watch`, `campaign-swarm`. Subsequent tiers ship as PRs against the same module per the rollout table.

### Job queue + dispatcher (`api/internal/queue/` + `api/internal/dispatcher/`)

- Durable queue. Start small: SQLite + litestream replication to S3-compatible storage. Evolve to Postgres + LISTEN/NOTIFY when traffic warrants. Avoid Redis/Kafka day-one.
- Dispatcher pulls a job, invokes the operator plane via the tunneled JSON-RPC (`fleetctl serve`), polls the operator-plane ledger for status, updates the job state machine in the cloud DB.
- Idempotency: every customer submission gets an idempotency key; replays return the same job, not a duplicate.

### Billing (`api/internal/billing/`)

- **Source of truth**: `internal/ledger` signed events emitted by the operator plane. The cloud's billing module subscribes via the tunneled JSON-RPC.
- **Stripe integration**: each ledger event of type `bill` produces a Stripe usage record (metered subscription product). Subscription product per SKU; usage metric varies (per-build / per-campaign / per-ticket / per-GB).
- **Daily reconciliation**: every 24h, compare ledger sum vs. Stripe reported usage; alert on drift > 0.5 %.
- **Cost caps**: per-tenant monthly cap (defaulted, customer-adjustable). Cap reached → `/v1/jobs` returns 402 with retry-after.

### Customer dashboard (`owera-cloud/web/`)

- Next.js App Router. Pages: `/dashboard` (recent jobs, usage, cost), `/jobs` (full history), `/jobs/[id]` (timeline + ledger + outputs), `/billing` (Stripe Customer Portal embed), `/api-keys` (CRUD), `/support` (ticket inbox).
- Design system: minimal — Tailwind + a small component lib (radix-ui primitives or shadcn/ui).
- API client generated from `openapi.yaml`.
- Real-time job updates via Server-Sent Events from the API (no Websockets initially).

### Status page (`owera-cloud/status/`)

- Public; reads metrics emitted by `internal/metrics` (Phase 2) via the same tunneled JSON-RPC.
- Components: API uptime, gateway reachability, per-SKU success rate (rolling 24 h), per-SKU SLA breach incidents.
- Static-generated nightly with a 60-second incident overlay refresh.

### Audit log (`api/internal/audit/`)

- Every state-changing action (sign-up, key creation, job submit, cancel, billing adjustment) writes an audit row keyed by `tenant_id`, `user_id`, `action`, `target`, `ts`, `ip`, `user_agent`. Append-only.
- Retention 7 years to satisfy SOC 2 trajectory. WORM storage (S3 with object-lock).

### Compliance trajectory (`owera-cloud/compliance/`)

- Not a code module — a docs + process workstream. SOC 2 Type 1 target: 12 months post-GA. Policies (security, access, incident response, data retention, change management) written in `compliance/policies/`. Audit-control mapping in `compliance/audit-controls/`.
- The Phase-2 primitives (signed ledger, audit log, secret handling, signed config sync, alerting, metrics) are most of the technical evidence. The compliance workstream wires them into formal controls.

## Phase 4: Launch readiness

- **Staging fleet**: a separate `claw-staging.local` Mac (or VM) provisioned via the same `fleetctl bootstrap-worker` flow. The cloud has a `staging` environment that points at it. All customer-impacting changes deploy to staging first.
- **Runbooks**: incident response per SKU (what does "campaign-swarm degraded" mean to operator and customer?), on-call playbook, escalation tree.
- **On-call rotation**: even with one operator (Rodrigo), explicit on-call hours + a pager (PagerDuty) wired to the alerting stack.
- **Customer onboarding flow**: signup → email verification → API key issuance → dashboard tour → first sandbox job (free quota of 1 job per SKU) → billing setup.
- **Pricing experimentation**: Stripe makes plan changes easy. The plan tracks 2 ramp points where pricing is re-evaluated (after first paying customer; after 3 paying customers).
- **Beta → GA gates**: 3 paying design partners ≥ 30 days each; SLA met for ≥ 95 % of jobs over the last 30 days; one full incident drill executed; audit log has zero cross-tenant access events.

## Decomposition: specialized agents, workstreams, and PR-sized tickets

The plan above describes *what* to build. This section describes *how a team of specialized agents could deliver it in parallel*. Three layers: **agents** (who), **workstreams** (cohesive deliverables), **tickets** (PR-sized units of work with explicit inputs, outputs, and an acceptance test).

### Agent roster (20 specialized roles across all phases)

**Operator-plane roles (Phases 1–2):** TL, CPE, BTE, MPE, DOE, HIE, TE, TRE, DWE, TME, DRE, QVE.
**Customer-plane roles (Phases 3–4):** PM, APE, IDE, BLE, FEE, CSE, SRE, SOE.

| Role | Charter | Phase 1 ownership | Phase 2 ownership |
|------|---------|-------------------|-------------------|
| **TL** — Tech Lead | Architecture, PR review, integration, cross-cutting decisions | All cross-cutting | All cross-cutting |
| **CPE** — Core Platform Engineer | Go lib foundations + cobra dispatcher + skill generation | `internal/{log,json,nodes,report}` + `cmd/fleetctl/main.go` + `commands/{state,gen_skills}.go` | — |
| **BTE** — Bootstrap & Transport Engineer | Gateway-to-worker plumbing | `internal/ssh`, `internal/bootstrap`, `remote/phase*.sh`, `remote/with_pw.exp`, `commands/bootstrap.go` | — |
| **MPE** — macOS Platform Engineer | launchd lifecycle + agent installers | `internal/launchd`, `templates/launchd/*`, `commands/setup_agent.go` | `internal/logaggregate` + log-aggregation LaunchAgent |
| **DOE** — Daily Ops & Audit Engineer | Read paths: delegate / health / smoke / audit | `internal/audit`, `commands/{delegate,health,smoke,audit}.go` | — |
| **HIE** — Hermes Integration Engineer | Anything wrapping `hermes` CLI | `internal/hermes`, `commands/{review,research,update}.go` | `internal/hermesjobs`, `commands/cronjob.go` |
| **TE** — Test Engineer | Scenario schema + runner + scenario authoring | `internal/scenarios`, `commands/test.go`, `scenarios/{smoke,usecase,readiness,e2e}.yaml` | `scenarios/usecase{31..34}.yaml` |
| **TRE** — Trust Engineer | Crypto + accountability primitives | — | `internal/{ledger,secrets,pairing,budget}`, `commands/{ledger,run-with-secrets,pair}.go` |
| **DWE** — Distribution & Workflow Engineer | Signed pushes, swarm, ETL | — | `internal/{configsync,skillsync,orchestrator,markers}`, `commands/{config-sync,skill-sync,swarm}.go` |
| **TME** — Telemetry Engineer | Alerts + metrics | — | `internal/{alerting,metrics}`, `commands/{alert,metrics}.go` |
| **DRE** — Docs & Release Engineer | README, docs/, archive, CI, releases | `README.md`, `docs/{operation,plan,security,roadmap}.md`, `docs/archive/`, `.github/workflows/{ci,release}.yml` | Doc maintenance + Phase 2 doc updates |
| **QVE** — QA & Verification Engineer | End-to-end live-fleet runs + drift sign-off gates | Phase 1 verification | Phase 2 verification |
| **PM** — Product Manager | SKU definitions, pricing tiers, customer interviews, beta-cohort selection, GA gates | — | Catalog content, pricing.md, onboarding.md, beta playbook |
| **APE** — API/Backend Engineer | Public API gateway + queue + dispatcher + catalog | — | `owera-cloud/api/internal/{catalog,jobs,queue,dispatcher}`, `cmd/apiserver/main.go`, `openapi.yaml` |
| **IDE** — Identity Engineer | Tenants, users, API keys, multi-tenancy, OAuth | — | `api/internal/{auth,identity}` |
| **BLE** — Billing Engineer | Stripe metering, reconciliation, cost caps, invoices | — | `api/internal/billing`, Stripe products + meters, daily-reconciliation job |
| **FEE** — Frontend Engineer | Customer dashboard + onboarding UX | — | `owera-cloud/web/*`, OpenAPI-generated client |
| **CSE** — Compliance & Security Engineer | SOC 2 trajectory, audit log, data retention, secrets policy | — | `api/internal/audit`, `compliance/*`, key rotation runbooks |
| **SRE** — Site Reliability Engineer | Deploys, on-call, status page, runbooks, staging fleet | — | `infra/*`, `status/*`, on-call rotation, incident drills |
| **SOE** — Solutions / Support Engineer | First-customer onboarding, in-product support inbox, success comms | — | `web/.../support`, `docs/support.md`, customer comms templates |

### Workstreams (12)

| WS | Name | Lead | Phase | Tickets | Blocking deps |
|----|------|------|-------|---------|---------------|
| WS-1 | Foundation libs + dispatcher | CPE | 1 | T1.1–T1.6 | — |
| WS-2 | Bootstrap & SSH plumbing | BTE | 1 | T2.1–T2.5 | T1.2 (log schema) |
| WS-3 | macOS platform (launchd lifecycle) | MPE | 1 | T3.1–T3.3 | T1.4 (report) |
| WS-4 | Daily ops & audit commands | DOE | 1 | T4.1–T4.4 | T1.{2,3,4}, T2.1 |
| WS-5 | Hermes-CLI integration | HIE | 1 | T5.1–T5.3 | T1.2, T2.1 |
| WS-6 | Scenario runner & Phase-1 tiers | TE | 1 | T6.1–T6.4 | T4.{1,2,3}, T5.2 |
| WS-7 | Docs, archive, CI | DRE | 1 | T7.1–T7.4 | — |
| WS-8 | Trust primitives | TRE | 2 | T8.1–T8.5 | Phase 1 complete |
| WS-9 | Distribution & workflow | DWE | 2 | T9.1–T9.4 | T8.1 (key infra) |
| WS-10 | Telemetry | TME | 2 | T10.1–T10.3 | Phase 1 complete |
| WS-11 | Use-case scenarios + log aggregation | TE + MPE | 2 | T11.1–T11.5 | WS-8, WS-9, WS-10 |
| WS-12 | Live-fleet verification | QVE | both | T12.1–T12.6 | gates at end of each phase |
| WS-13 | Product framing & SKU catalog content | PM | 3 | T13.1–T13.4 | — |
| WS-14 | Public API + catalog + queue + dispatcher | APE | 3 | T14.1–T14.6 | Phase 2 ledger + pairing + metrics |
| WS-15 | Identity & multi-tenancy | IDE | 3 | T15.1–T15.4 | T14.1 (API scaffold) |
| WS-16 | Billing (Stripe) | BLE | 3 | T16.1–T16.5 | T14.2 (job lifecycle), T8.1 (ledger), T15.1 (tenants) |
| WS-17 | Customer dashboard + onboarding UX | FEE | 3 | T17.1–T17.5 | T14.1 (openapi), T15.1 (auth), T16.2 (Stripe portal) |
| WS-18 | Compliance baseline + audit log | CSE | 3 | T18.1–T18.5 | T14.1, T15.1, T8.1 |
| WS-19 | Edge, infra, status page, on-call | SRE | 3+4 | T19.1–T19.6 | T14.1, T10.2 (metrics) |
| WS-20 | Beta onboarding + customer support | SOE | 4 | T20.1–T20.4 | WS-17, WS-19 |

### Ticket backlog (PR-sized units)

Each ticket is one PR, ~1–3 days of work for the owner, with an acceptance test the reviewer can run.

#### WS-1: Foundation libs + dispatcher — Owner: CPE

- **T1.1** — Create `owera` GitHub org; transfer `rrecio/hermes-setup` → `owera/hermes-setup` (archive on transfer). Bootstrap `owera/owera-fleet` (public): `go.mod`, `.gitignore`, `LICENSE` (carried), `README.md` stub, `CLAUDE.md` (revised), `.claude/settings.json` with gstack PreToolUse hook, `CONTRIBUTING.md`, `SECURITY.md`, branch protection on `main` (require PR + CI green + 1 review). *Accept:* `go mod tidy` clean; gstack hook fires; `gh repo view owera/owera-fleet` returns public + protected.
- **T1.2** — `internal/log`: JSONL schema `{ts,node,phase,action,result,duration_ms,stderr_tail}`. Unit-test 3 result types. *Accept:* Output shape matches today's `delegate.jsonl`.
- **T1.3** — `internal/nodes`: read `nodes.txt`; one/all/random iteration. *Accept:* Returns live 2-node fleet correctly.
- **T1.4** — `internal/json` + `internal/report` (markdown header/footer). *Accept:* Renders match today's `~/.hermes/reports/*.md`.
- **T1.5** — `cmd/fleetctl/main.go` cobra scaffold + `commands/state.go`. *Accept:* `fleetctl state --markdown` ≡ STATE.md (timestamps differ).
- **T1.6** — `Skill()` interface + `commands/gen_skills.go`. *Accept:* `--check` fails on drift; clean run emits 11 SKILL.md files.

#### WS-2: Bootstrap & SSH — Owner: BTE

- **T2.1** — `internal/ssh` (`golang.org/x/crypto/ssh`); Keychain integration. *Accept:* `fleetctl smoke hermes@claw1.local` reachable.
- **T2.2** — `remote/with_pw.exp`: one-shot sudo handoff via private fd. *Accept:* Used once in Phase 2; never re-invoked after Phase 6.
- **T2.3** — `remote/phase00_brew_baseline.sh` (Homebrew + essentials). Idempotent. *Accept:* Re-run reports `no-change`; shellcheck clean.
- **T2.4** — `internal/bootstrap` orchestrator + `remote/phase{02..09}*.sh`. *Accept:* Clean macOS VM provisioned ≤10 min; re-run `no-change`.
- **T2.5** — `commands/bootstrap.go` (`fleetctl bootstrap-worker hermes@<host>`). *Accept:* Heartbeat <2 min old at exit; re-run idempotent.

#### WS-3: macOS platform — Owner: MPE

- **T3.1** — `internal/launchd` (render/bootstrap/bootout/status); Go-template substitutions on 5 plists. *Accept:* Byte-identical to today's `com.hermes.*.plist`.
- **T3.2** — `commands/setup_agent.go` subcommands `backup`, `backup-worker`, `watchdog`, `logrotate`, `log-aggregate`. *Accept:* All 5 LOADED in `launchctl print`.
- **T3.3** — Re-run idempotency. *Accept:* Post-install re-run reports `no-change`; no duplicates.

#### WS-4: Daily ops & audit — Owner: DOE

- **T4.1** — `commands/delegate.go`. *Accept:* JSONL output matches today's `delegate.jsonl`.
- **T4.2** — `commands/health.go` (snapshot + diff, `--diff <prev>`). *Accept:* Markdown matches `fleet-health-snapshot.sh`.
- **T4.3** — `commands/smoke.go`. *Accept:* PASS/WARN/FAIL exit codes match `smoke-test-node.sh`.
- **T4.4** — `internal/audit` + `commands/audit.go`. *Accept:* `audit config` reproduces SECURITY_NOTES.md drift table.

#### WS-5: Hermes-CLI integration — Owner: HIE

- **T5.1** — `internal/hermes` (version-pin enforcement, invocation wrapper, secrets-stdin). *Accept:* `fleetctl version-check` returns pinned tag.
- **T5.2** — `commands/review.go`. *Accept:* Markdown shape matches `review-branch.sh`.
- **T5.3** — `commands/research.go` + `commands/update.go --dry-run`. *Accept:* `research` produces report; `update --dry-run` lists planned changes.

#### WS-6: Scenario runner & Phase-1 tiers — Owner: TE

- **T6.1** — Scenario YAML schema + `internal/scenarios` loader + assertion engine. *Accept:* Malformed YAML errors with line numbers.
- **T6.2** — `commands/test.go --tier=...`. *Accept:* `test --tier=smoke` runs minimal scenario.
- **T6.3** — Phase-1 scenarios `{smoke,usecase,readiness,e2e}.yaml`. *Accept:* All tiers green at parity (≥7/10 usecase, ≥4/5 e2e).
- **T6.4** — GAP-to-roadmap auto-stub. *Accept:* Re-runs deduplicate; new GAP appears once.

#### WS-7: Docs, archive, CI — Owner: DRE

- **T7.1** — Snapshot `hermes-setup` into `docs/archive/hermes-setup-2026-05-16/`. **Run `gitleaks` over the archived tree before commit** to confirm no secrets leak into a public repo. *Accept:* Full tree preserved + git-tracked; gitleaks clean.
- **T7.2** — `docs/{operation,plan,security,roadmap}.md` (v2 + review collapsed). Archive v1/v1-review/services-guide/data-prevention/important-notes. *Accept:* Cross-doc refs resolve; archive reachable.
- **T7.3** — `.github/workflows/ci.yml` (Go test + lint + shellcheck + skill-drift + **gitleaks secret scan + license scan**, both repos). *Accept:* Trivial-PR check green; synthetic PR with a fake token fails gitleaks gate.
- **T7.4** — `.github/workflows/release.yml`: cross-compile darwin/{arm64,amd64} + checksums. *Accept:* Tag → release with 2 binaries.

#### WS-8: Trust primitives — Owner: TRE  *(Phase 2)*

- **T8.1** — `internal/ledger`: per-task signed JSONL; minisign key in Keychain; replay+compensate API. *Accept:* Signed entry verifies; tampered fails.
- **T8.2** — `commands/ledger.go` (`inspect`/`verify`/`replay`/`compensate`). *Accept:* All 4 subcommands operate on sample ledger.
- **T8.3** — `internal/secrets` + `commands/run-with-secrets.go` (stdin-fed; never argv/env/disk). *Accept:* `ps auxe` during run shows no plaintext.
- **T8.4** — `internal/pairing` + `commands/pair.go`. *Accept:* Revoked pairing fails subsequent `delegate`; signature-bypass rejected.
- **T8.5** — `internal/budget`: per-pairing rate limit + CPU-seconds/day + cost cap; ledger-backed. *Accept:* Over-limit returns clear error; reconciles with billing.

#### WS-9: Distribution & workflow — Owner: DWE  *(Phase 2)*

- **T9.1** — `internal/configsync` + `commands/config-sync.go` (minisign-signed bundles). *Accept:* Tampered rejected; legitimate applies + logs manifest hash.
- **T9.2** — `internal/skillsync` + `commands/skill-sync.go` (signed Git commits + node-side pull). *Accept:* Tampered commit rejected on node.
- **T9.3** — `internal/orchestrator` + `commands/swarm.go` (parent + N leaves; per-worker ledger merge; retry/compensation). *Accept:* `swarm --workers 3` produces 3 per-worker ledgers + 1 merged report.
- **T9.4** — `internal/markers` (step.done content-hashed; freshness + integrity). *Accept:* Stale rejected; tampered rejected; valid accepted.

#### WS-10: Telemetry — Owner: TME  *(Phase 2)*

- **T10.1** — `internal/alerting` + `commands/alert.go` (PagerDuty + OpsGenie + ntfy.sh; pager-first). *Accept:* `alert test --severity high` pages within 30s.
- **T10.2** — `internal/metrics` + `commands/metrics.go` (Prometheus; 4 baseline metrics). *Accept:* `curl localhost:9090/metrics` returns valid output.
- **T10.3** — Re-wire watchdog + bootstrap-failure paths through `internal/alerting`. *Accept:* Watchdog pages PagerDuty; ntfy.sh gets secondary signal.

#### WS-11: Use-case scenarios + log aggregation — Owners: TE + MPE  *(Phase 2)*

- **T11.1** — `internal/logaggregate` + log-aggregation LaunchAgent (nightly `rsync --remove-source-files`). *(MPE)* *Accept:* Overnight: node logs shrink, gateway grows, integrity verified.
- **T11.2** — `scenarios/usecase31_native_app.yaml` (v2 §3.1). *(TE; deps: T8.x, optional T9.3)* *Accept:* pairing → delegate → ledger → `gh pr create` → alerting on failure.
- **T11.3** — `scenarios/usecase32_marketing_swarm.yaml` (v2 §3.2). *(TE; deps: T9.3, T8.1)* *Accept:* 3 leaves + per-worker ledgers + idempotency-key compensation.
- **T11.4** — `scenarios/usecase33_ticket_triage.yaml` (v2 §3.3). *(TE; deps: hermesjobs T5.x ext, T8.3, T10.1)* *Accept:* 2-min poll; priority>8 pages PagerDuty.
- **T11.5** — `scenarios/usecase34_etl.yaml` (v2 §3.4). *(TE; deps: T9.4, T8.1, hermesjobs)* *Accept:* Parallel delegate across 3 nodes; markers verified; ledger resume on partial failure.

#### WS-12: Live-fleet verification — Owner: QVE  *(both phases)*

- **T12.1** — Bootstrap-test on fresh macOS VM. *(Phase-1 gate)*
- **T12.2** — State-equivalence: `fleetctl state --markdown` ≡ STATE.md. *(Phase-1 gate)*
- **T12.3** — Phase-1 tiers all green at parity. *(Phase-1 gate)*
- **T12.4** — Drift equivalence: `fleetctl audit config` ≡ SECURITY_NOTES.md drift table. *(Phase-1 gate)*
- **T12.5** — Backup round-trip (install → snapshot count +1 → restore byte-identical).
- **T12.6** — Phase-2 e2e: all 4 Use-Case scenarios green on live fleet. *(Phase-2 gate)*

#### WS-13: Product framing & SKU catalog content — Owner: PM  *(Phase 3)*

- **T13.1** — Finalize V0 SKU definitions (triage-watch, campaign-swarm): scope, inputs schema, pricing, SLA, success metrics. *Accept:* Both approved by TL + Rodrigo; specs land in `docs/pricing.md`.
- **T13.2** — Run 5 customer-discovery interviews with prospective beta clients across the 5 catalog categories; capture pain points + willingness-to-pay + SKU demand signal. *Accept:* Notes summarized; 1–2 design partners identified; V1/V2 SKU priority signal captured.
- **T13.3** — Author `docs/onboarding.md` (new-customer ramp) and `docs/support.md` (canned response templates, SLA commitments). *Accept:* DRE + SOE review pass.
- **T13.4** — Define GA gates (paying-customer count, SLA threshold, drill cadence) and codify in `compliance/policies/ga-gate.md`. *Accept:* TL sign-off; gates referenced by WS-20.
- **T13.5** — Author V1 SKU specs (research-brief, code-audit) for GA wave. *Accept:* Both ready for catalog PRs at start of Wave 10; specs in `docs/pricing.md`.
- **T13.6** — Publish SKU template + contribution guide: `docs/sku-template.md` showing the one-PR contract (file structure, schema, pricing, dispatcher pattern, scenario). *Accept:* A second engineer can author a draft SKU spec from the template in <2 h.
- **T13.7** — Roadmap V2–V4 SKU rollout sequence as a tracked document with demand signals + entry criteria per SKU. *Accept:* Lives in `docs/roadmap.md` under a `catalog/` subsection; reviewed quarterly post-GA.

#### WS-14: Public API + catalog + queue + dispatcher — Owner: APE  *(Phase 3)*

- **T14.1** — Create `owera/owera-cloud` (public) under the `owera` org with branch protection + CI inheritance. Scaffold `owera-cloud/api/`. `cmd/apiserver/main.go` boots, `/healthz` returns 200, `openapi.yaml` skeleton committed (title: "Owera Agentic API"). *Accept:* `gh repo view owera/owera-cloud` returns public + protected; `fly deploy --remote-only` yields a live endpoint behind a placeholder domain.
- **T14.2** — Job lifecycle state machine in `internal/jobs/`. CRUD endpoints `POST /v1/jobs`, `GET /v1/jobs/{id}`, `POST /v1/jobs/{id}/cancel`. *Accept:* Round-trip submit → status → cancel works against a mocked dispatcher; OpenAPI examples render in stoplight.
- **T14.3** — Durable queue in `internal/queue/` (SQLite + litestream). At-least-once delivery + idempotency-key dedup. *Accept:* 1,000-job synthetic-load test; no drops; replicas converge.
- **T14.4** — Catalog in `internal/catalog/`: SKU registry with `{Name, Version, Category, InputsSchema (JSON Schema), Pricing, SLA, Dispatcher, BillingMeter}`. Register both V0 SKUs (triage-watch, campaign-swarm). Enforce the one-PR contract via CI: a PR adding `internal/catalog/<sku>.go` must not touch other core files. *Accept:* `GET /v1/skus` returns 2 SKUs; invalid `inputs` rejected by JSON-Schema validation; CI rejects a synthetic PR that touches catalog + queue together.
- **T14.7** — Register V1 SKUs (research-brief, code-audit) as catalog PRs ahead of GA. Each PR is `<sku>.go` + scenario + docs only. *Accept:* `GET /v1/skus` returns 4 SKUs at GA; both pass acceptance-test scenarios in `owera-cloud/tests/scenarios/`.
- **T14.5** — Dispatcher in `internal/dispatcher/` calls `fleetctl serve` over the Cloudflare tunnel. Polls operator ledger for status; updates job state. *Accept:* End-to-end submit-via-API → operator-plane delegate → ledger event → job state advances to succeeded.
- **T14.6** — `fleetctl serve` (new operator-plane subcommand, owned by HIE+APE jointly) exposes minimal JSON-RPC over local UDS, surfaced through the tunnel. *Accept:* `curl --unix-socket` round-trip from cloud → operator → cloud.

#### WS-15: Identity & multi-tenancy — Owner: IDE  *(Phase 3)*

- **T15.1** — `internal/identity/` tenants + users + API keys (hashed with argon2id; never store plaintext). Row-level `tenant_id` enforcement. *Accept:* Cross-tenant access attempted in test → denied; no leakage in audit log diff.
- **T15.2** — `internal/auth/` API-key middleware. *Accept:* Bad key → 401 with structured error; good key resolves tenant.
- **T15.3** — OAuth via Clerk (or WorkOS) for the dashboard. Sessions in HTTP-only cookies. *Accept:* Dashboard sign-in works; sessions revocable.
- **T15.4** — Cross-tenant CI safety net: dedicated test attempts to fetch another tenant's job; assert 404 (not 403) to avoid leaking existence. *Accept:* CI green; explicit attack-path test in `tests/`.

#### WS-16: Billing (Stripe) — Owner: BLE  *(Phase 3)*

- **T16.1** — Create Stripe products: `triage-watch` (metered, per ticket), `campaign-swarm` (per campaign), test prices. Use Stripe MCP tools for setup (`mcp__claude_ai_Stripe__create_product` + `create_price`). *Accept:* Products exist in Stripe test mode; prices match `docs/pricing.md`.
- **T16.2** — Stripe Customer Portal embed in dashboard route `/billing`. *Accept:* Customer can update card, view invoices, cancel subscription.
- **T16.3** — `internal/billing/` subscribes to `internal/ledger` events of type `bill` over the tunnel; emits Stripe usage records via `stripe_api_execute`. *Accept:* One ledger event → one usage record; idempotent on retry (Stripe idempotency-key).
- **T16.4** — Daily reconciliation job: ledger sum vs. Stripe reported usage. Alert via TME alerting on drift > 0.5 %. *Accept:* Synthetic drift triggers alert; no drift → silent run.
- **T16.5** — Cost cap: tenant monthly spend cap (default + customer-adjustable). Exceeded → `POST /v1/jobs` returns 402 with retry-after. *Accept:* Synthetic over-cap test returns 402; cap reset at billing period rollover.

#### WS-17: Customer dashboard + onboarding UX — Owner: FEE  *(Phase 3)*

- **T17.1** — Next.js App Router scaffold + Tailwind + shadcn/ui. Generated API client from `openapi.yaml`. *Accept:* `pnpm dev` boots; `/dashboard` renders mock data.
- **T17.2** — `/dashboard`, `/jobs`, `/jobs/[id]` routes with real API. SSE for job timeline. *Accept:* Submit a job from CLI; see it appear and progress in the dashboard.
- **T17.3** — `/api-keys` CRUD (create, view-once, revoke). *Accept:* Key creation shows once; revocation invalidates immediately.
- **T17.4** — `/billing` Stripe Customer Portal embed; `/usage` current-period meter. *Accept:* Card update flow works; usage updates after a job completes.
- **T17.5** — `/support` ticket inbox (lightweight: customer posts + operator responds). *Accept:* End-to-end ticket open → operator reply → customer notify.

#### WS-18: Compliance baseline + audit log — Owner: CSE  *(Phase 3)*

- **T18.1** — `internal/audit/` append-only log; every state-changing API call writes a row. WORM target (S3 with object-lock). *Accept:* `POST /v1/jobs` writes an audit row; row tamper attempt fails to update.
- **T18.2** — Data retention policy: per-table retention + deletion API. Customer-data-delete endpoint satisfying **both LGPD Art. 18 (Brazilian) and GDPR Art. 17 (EU) right-to-erasure**. *Accept:* `DELETE /v1/tenants/me/data` queues a deletion job; verifiable in audit log; deletion completes within the LGPD/GDPR-compliant window (≤30 days from request).
- **T18.3** — Security policies in `compliance/policies/`: access control, incident response, data classification, change management, vendor management, **LGPD compliance (controller obligations, ANPD notification process, data-protection officer designation)**. *Accept:* All 6 policies committed and reviewed; LGPD policy references Owera Software Ltda as controller and names the DPO.
- **T18.4** — Audit-control mapping in `compliance/audit-controls/`: SOC 2 Common Criteria → which technical control + evidence file. *Accept:* Each CC has a mapped evidence path that exists.
- **T18.5** — Key rotation runbook: minisign keys (Phase 2 ledger/configsync) + Stripe API keys + Cloudflare tokens. Quarterly rotation cadence. *Accept:* First rotation drill executed end-to-end.

#### WS-19: Edge, infra, status page, on-call — Owner: SRE  *(Phases 3+4)*

- **T19.1** — Cloudflare account + DNS for `owera.com` and `owera.ai` + Tunnel from gateway Mac to cloud. Provision subdomains: `api.owera.ai`, `app.owera.ai` (dashboard), `status.owera.ai`. `owera.com` for corporate / marketing (handled outside this plan). *Accept:* Tunnel survives gateway restart; `curl https://api.owera.ai/healthz` returns 200.
- **T19.2** — `owera-cloud/api` deployed to Fly.io (or Cloud Run) with secrets injected from a chosen secret manager. *Accept:* Blue/green deploy with zero-downtime cutover; rollback works.
- **T19.3** — `owera-cloud/web` deployed to Vercel (or equivalent). *Accept:* Preview deploys per PR; production behind custom domain.
- **T19.4** — `status/` public status page driven by `internal/metrics`. *Accept:* Forced incident (kill gateway) reflects in <60 s.
- **T19.5** — Staging fleet provisioned (`fleetctl bootstrap-worker hermes@claw-staging.local`); cloud `staging` environment points at it. *Accept:* All Phase-2 scenarios pass on staging.
- **T19.6** — PagerDuty rotation + first incident drill (simulated gateway outage). *Accept:* Pager fires; on-call follows runbook; post-mortem written.

#### WS-20: Beta onboarding + customer support — Owner: SOE  *(Phase 4)*

- **T20.1** — Onboarding playbook for design partner #1. *Accept:* Customer signs MSA, gets keys, runs first job within 7 days.
- **T20.2** — Support inbox SLAs (first response 24 h business hours). *Accept:* 30-day rolling metric ≥ 90 %.
- **T20.3** — Incident comms templates (degradation, outage, SLA breach). *Accept:* Used in T19.6 drill.
- **T20.4** — Beta-to-GA review at 90 days. *Accept:* PM + TL + Rodrigo sign off on GA decision against the gates from T13.4.

### Coordination plan (waves, parallelism, blockers)

```
WAVE 1 — fully parallel, blocked by nothing
  WS-1 (CPE):  T1.1 → T1.2 → T1.3 → T1.4 → T1.5
  WS-2 (BTE):  T2.2, T2.3 against mocked log iface; T2.1+ wait on T1.2
  WS-7 (DRE):  T7.1 (archive) + T7.3 (CI scaffold)

WAVE 2 — starts when T1.2/T1.3/T1.4 merged
  WS-2 (BTE):  T2.1 → T2.4 → T2.5
  WS-3 (MPE):  T3.1 → T3.2 → T3.3
  WS-4 (DOE):  T4.1 → T4.2 → T4.3 → T4.4
  WS-5 (HIE):  T5.1 → T5.2 → T5.3
  WS-7 (DRE):  T7.2 + T7.4

WAVE 3 — starts when WS-4 + WS-5 daily-use commands land
  WS-6 (TE):   T6.1 → T6.2 → T6.3 → T6.4
  WS-1 (CPE):  T1.6 final wiring

WAVE 4 — PHASE-1 VERIFICATION GATE
  WS-12 (QVE): T12.1, T12.2, T12.3, T12.4
  TL: cut-over decision (now? or after Phase 2?)

WAVE 5 — Phase 2 primitives, fully parallel
  WS-8 (TRE):  T8.1 → T8.2 → T8.3 → T8.4 → T8.5
  WS-9 (DWE):  T9.1 (shares key infra w/ T8.1) → T9.2 → T9.3 → T9.4
  WS-10 (TME): T10.1 → T10.2 → T10.3
  WS-5 (HIE):  hermesjobs extension (T5.x for cronjob wrapper)

WAVE 6 — use-case scenarios + log aggregation
  WS-11 (TE+MPE): T11.1, T11.2, T11.3, T11.4, T11.5
  WS-12 (QVE):    T12.5, T12.6 — PHASE-2 GATE

CUTOVER — hermes-setup ops → owera-fleet (operator plane fully on Go runtime)

WAVE 7 — Phase 3 product framing + cloud scaffolding (parallel)
  WS-13 (PM):  T13.1 (V0 SKUs), T13.2 (customer-discovery), T13.4 (GA gates), T13.6 (SKU template)
  WS-14 (APE): T14.1 (API scaffold), T14.6 jointly with HIE (fleetctl serve)
  WS-19 (SRE): T19.1 (Cloudflare tunnel), T19.2 (API deploy stub)

WAVE 8 — Phase 3 core build (parallel after Wave 7)
  WS-14 (APE): T14.2 → T14.3 → T14.4 → T14.5
  WS-15 (IDE): T15.1 → T15.2 → T15.3 → T15.4
  WS-16 (BLE): T16.1 → T16.2 → T16.3 → T16.4 → T16.5  (needs T14.2, T15.1, T8.1)
  WS-18 (CSE): T18.1 → T18.2 → T18.3, T18.4, T18.5
  WS-17 (FEE): T17.1 → T17.2 → T17.3 → T17.4 → T17.5 (needs T14.* + T15.*)
  WS-19 (SRE): T19.3, T19.4

WAVE 9 — Phase 4 launch readiness
  WS-19 (SRE): T19.5 (staging fleet), T19.6 (incident drill)
  WS-20 (SOE): T20.1 (onboard design partner #1), T20.2, T20.3
  WS-13 (PM):  T13.3 (onboarding + support docs)
  PRIVATE BETA OPENS — first paid customer

WAVE 10 — Beta → GA
  WS-20 (SOE): T20.4 (90-day beta review)
  WS-13 (PM):  T13.5 (V1 SKU specs) → pricing iteration based on beta data
  WS-14 (APE): T14.7 (register V1 SKUs in catalog)
  WS-18 (CSE): SOC 2 Type 1 readiness assessment
  GA DECISION GATE (PM + TL + Rodrigo) — V1 catalog (4 SKUs) live at GA

POST-GA — continuous catalog growth
  Each new SKU = one PR per the extensibility contract (T13.6 template)
  V2 (90 d post-GA): + dep-upgrade, inbox-triage, monitor-watch, content-batch
  V3 (180 d post-GA): + xcode-ci, app-build, docs-author, incident-postmortem
  V4 (12 mo+): demand-driven — test-author, migration-pilot, lead-enrich, etl-flow
```

### Definition of done

- **Per ticket**: PR merged, CI green, acceptance test passing, owner + TL agreed.
- **Per workstream**: All tickets done + WS-level integration test green + skill manifest in sync.
- **Phase 1**: WS-1..7 complete; WS-12 T12.1–T12.4 green. `fleetctl` operates the fleet at parity with today.
- **Phase 2**: WS-8..11 complete; WS-12 T12.5–T12.6 green. The four v2 §3 use cases deliverable end-to-end.
- **Phase 3**: WS-13..19 complete; first synthetic end-to-end paid job (API submit → operator-plane run → Stripe usage record → invoice generated) green. Cross-tenant access test green. Status page reflects fleet state.
- **Phase 4**: WS-20 complete + T19.5 (staging) + T19.6 (drill) green; ≥1 design partner running paid jobs ≥ 30 days; SLA met for ≥ 95 % of jobs in last 30 days; zero cross-tenant audit events. **Beta complete; GA decision green-lit.**

### Critical handoffs & integration points

- **CPE → all**: `internal/log`, `internal/report`, `internal/nodes` are upstream deps. Lock the public Go interfaces *before* Wave 2 so other agents can code against signatures while CPE finishes implementation.
- **BTE → DOE/HIE/TE**: `internal/ssh` is the dependency. Freeze interface at end of Wave 1.
- **TRE ↔ HIE**: `internal/secrets` API must satisfy both — TRE owns wrapper, HIE consumes in `commands/{review,research}`. Joint design at start of Wave 5.
- **TRE ↔ DWE**: Signed bundles in `configsync` + `skillsync` reuse minisign key infra from `internal/ledger`. *One* keystore module. TL enforces.
- **TE ↔ Phase-2 owners**: Each Phase-2 owner authors a draft scenario fragment showing their primitive in use; TE assembles WS-11 from fragments + cross-cutting glue.
- **QVE → all**: Gate-keeper. Two named gates (end-of-Wave-4, end-of-Wave-6). No cutover without QVE sign-off + TL approval.

### Mapping to Claude/Agent SDK subagents

If this team is actually run as Claude `Agent` subagents:

| Agent role | Suggested subagent_type | Notes |
|------------|-------------------------|-------|
| TL | `general-purpose` or `Plan` | Orchestrates, reviews, integrates |
| CPE, BTE, MPE, DOE, HIE, TRE, DWE, TME | `general-purpose` with full tool surface | Each scoped to one workstream + interface contracts |
| TE | `general-purpose` | Scenario authoring + runner; lighter tool surface |
| DRE | `general-purpose` | Docs + CI; mostly Edit/Write |
| QVE | `Explore` for read-only audits + `general-purpose` for live drives | Strictly verification, no write authority |
| PM | `general-purpose` | SKU + pricing docs; customer-discovery synthesis |
| APE, IDE, BLE, FEE | `general-purpose` | Phase-3 cloud build; FEE needs `Bash` for `pnpm` |
| CSE | `general-purpose` + `Explore` | Audit log + policies; audit-control mapping |
| SRE | `general-purpose` | Infra deploys; ideally with restricted prod write |
| SOE | `general-purpose` + `claude_ai_Gmail` / `claude_ai_Slack` MCP | Customer comms over real channels |

Each subagent is spawned with: (1) its workstream charter, (2) the locked interface contracts it depends on, (3) explicit acceptance tests, (4) "stop and report" instructions on blockers. The Tech Lead is the only role with cross-workstream authority and is the integration funnel.

## Tradeoffs explicitly accepted

- **Go toolchain on the gateway** is a new dependency. Mitigated by shipping `fleetctl` as a cross-compiled darwin/arm64 + darwin/amd64 static binary in GitHub releases — operators install via `curl | tar -xz`, no Go SDK needed on the gateway.
- **Bash 3.2 still binds the worker fragments.** The constraint isn't gone, just pushed to a smaller surface (`remote/*.sh`, ~10 files) where it actually matters.
- **CI burden grows.** Today's repo has no CI. The recreation introduces Go test + golangci-lint + shellcheck + skill-drift check + Phase-3 OpenAPI lint + cross-tenant attack tests. Worth it for the typed core and the customer-data surface.
- **Loss of git history per script** is the cost of going green-field instead of refactoring in-place. Preserved by archiving the full current tree under `docs/archive/hermes-setup-2026-05-16/`.
- **Cloud bill becomes a recurring cost** (Fly.io/Cloud Run + Vercel + Cloudflare + Stripe + Clerk/WorkOS + PagerDuty + S3-with-object-lock). Order-of-magnitude estimate: $200–$500/month at private-beta scale. Build into pricing tier from T13.1.
- **A second repo (`owera-cloud`) doubles the release surface.** Mitigated by separate CI per repo and a tight contract (the `fleetctl serve` JSON-RPC) that both sides depend on.
- **The gateway Mac mini becomes a production dependency, not just a dev box.** It must be on UPS, have a wired connection + 4G/5G backup, be physically secured, and survive macOS auto-updates. **Macapá's residential/commercial infrastructure is less robust than São Paulo / US data centers**, so resilience controls matter more here. Documented in `compliance/policies/access.md` and the on-call runbook.
- **SOC 2 Type 1 is on a 12-month horizon.** That's a deliberate ramp — going for it earlier would consume the same engineering hours that ship Phase 3. The technical evidence (signed ledger, audit log, secret handling, signed config sync) is built in Phases 2-3 so the audit itself is mostly paperwork by month 12.
- **The customer plane introduces single points of failure** (Stripe, Clerk, Cloudflare). Mitigated by treating each as a vendor risk with a documented incident response in `compliance/policies/vendor.md` and a graceful-degradation mode (job queueing pauses, customer-visible status page reflects it).
- **Public repos surface our engineering choices**, including the embarrassing-in-the-moment bits (TODO comments, in-progress refactors, work-in-progress branches). The trade is intentional: public is a marketing surface (trust signal, recruiting, customer due-diligence) at the cost of doing engineering hygiene in the open. Discipline lives in `CONTRIBUTING.md` and code-review norms, not in private-by-default settings.

## Open questions for execution

These don't change the plan shape but should be resolved at the named gates:

**Phase 1/2 (operator plane):**

1. ~~**Repo names & visibility & GitHub org**~~ — **RESOLVED**. Names: `owera-fleet`, `owera-cloud`. Visibility: **public**. GitHub org: **new `owera` org** (create it; transfer `hermes-setup` from `rrecio/hermes-setup` → `owera/hermes-setup` as an archived repo; new repos land at `owera/owera-fleet` and `owera/owera-cloud`). Org-level CI secrets, branch-protection defaults, and security policies configured once and inherited by both repos.
2. **Cobra vs `flag` stdlib** for the dispatcher: cobra recommended.
3. **YAML library**: `gopkg.in/yaml.v3` (standard).
4. **Logging library**: `log/slog` stdlib + custom JSONL writer. No zap/zerolog.
5. **Cut-over date**: When the live fleet stops being operated through `hermes-setup` and starts being operated through `owera-fleet`. Suggest end of Wave 4 (Phase-1 gate) or end of Wave 6 (Phase-2 gate) — Rodrigo's call.

**Phase 3 (customer plane — block at start of Wave 7):**

6. ~~**Brand & domain**~~ — **RESOLVED**. Product brand is **"Owera Agentic"**. Domains: `owera.com` (corporate / marketing) and `owera.ai` (product surface — `api.owera.ai`, `app.owera.ai`, `status.owera.ai`). Email sending: `hello@owera.com` (sales/support), `noreply@owera.ai` (product transactional).
7. **Where does "product/growth" work live?** Marketing site, lead funnel, demos, pricing-page copy — this plan deliberately excludes them. Need a parallel track owned by Rodrigo + PM, with its own roadmap.
8. **Cloud hosting choice**: Fly.io (Mac-friendly latency, simple ops, $25–100/mo at scale) vs Cloud Run (Google trust signal, more cloud-native) vs Render (simplest deploy UX). Recommend Fly.io for private beta; revisit at GA.
9. **Identity vendor**: Clerk (best UX, mid-priced) vs WorkOS (enterprise-friendly, B2B SSO) vs roll-own (cheapest, highest engineering cost). Recommend Clerk for beta + WorkOS later if enterprise customers ask.
10. **Stripe product structure & currency**: One subscription per customer with multiple SKUs as metered components, or one subscription per SKU? Recommend the former (cleaner customer invoice). Confirm with BLE + PM during T16.1. **Currency**: start USD-only; add BRL once a Brazilian customer signs. **Stripe Brazil onboarding** is its own KYC; budget 2–4 weeks before billing can go live in BRL. Stripe USD billing can ship immediately under the BR entity with international receipts.
11. **Queue technology evolution path**: Start SQLite+litestream; when do we cut to Postgres? Probably at 5+ customers or 10K+ jobs/month. Don't optimize early.
12. **Legal**: MSA template, terms of service, **LGPD-compliant privacy policy**, **DPA** with **SCC annex for EU↔BR transfers**. Engagement with a Brazilian SaaS-experienced lawyer required (Macapá-based or São Paulo-based with remote engagement). All docs must reference Owera Software Ltda as the controller/processor. Need legal review before T20.1 onboarding the first paying customer. **Block at T20.1.**
13. ~~**Compliance scope**~~ — **PARTIALLY RESOLVED**. **LGPD mandatory from day one** (Brazilian operator); **GDPR** triggered by first EU customer (likely in beta); **SOC 2 Type 1** trajectory targets US enterprise demand. **HIPAA** out of scope until healthcare SKU exists. T18.2 (deletion API) covers LGPD + GDPR right-to-erasure together. *Still to decide:* whether the privacy policy is published bilingual (pt-BR + en) at launch or English-only with pt-BR follow-up.
14. **Domain expert dependency**: SOC 2 readiness needs an external auditor relationship (Vanta, Drata, or a direct firm). Decision at start of Wave 9.

**Phase 4 (launch readiness — block at start of Wave 9):**

15. **Design partner selection**: 3 prospective customers — who, what SKU, what compensation (free pilot? deep discount?)? Owned by PM (T13.2 → T20.1).
16. **On-call coverage model**: Only Rodrigo (24/7) is unsustainable. Hire a junior SRE? Use a small ops shop on retainer? Defer paying customers to business hours only? Affects pricing (SLA promise tier).
17. **Pricing iteration cadence**: First repricing review at first paying customer (T20.1 + 30 days). Second at 3 paying customers. Codify in `docs/pricing.md` change log.

**Brazil-specific (block at T16.1 / T20.1):**

19. **Brazilian tax-and-accounting setup**: ISS rate for software-as-a-service in Macapá (varies by municipality, ~2–5 %), PIS/COFINS treatment, classification as "software exports" if international (potentially exempt from ISS). Need a Brazilian tax accountant on retainer **before T16.1 (Stripe products created with prices)** so the prices in `docs/pricing.md` reflect net-after-tax economics. Without this, we set prices and discover margin is wrong after first invoice.
20. **Brazil business-banking + Stripe BR onboarding**: Stripe Brasil requires a CNPJ (Owera's tax ID), local bank account, and KYC docs. Allow 2–4 weeks lead time. **Start in parallel with Phase 3 build, not after.** Owned by Rodrigo + BLE.
21. **Macapá infrastructure resilience**: The gateway Mac mini lives in Macapá. Power and broadband reliability are lower than São Paulo / US data centers. Mitigations: UPS (already required by `compliance/policies/access.md`), 4G/5G backup connectivity, weekly off-site backup (already in Phase 1). If a paying customer raises uptime concerns, evaluate a cloud-Mac sidecar (MacStadium / AWS EC2 Mac) — defer until that demand is real.
22. **Customer-language strategy**: Beta launches **English-only**. Add pt-BR for dashboard + docs at the first Brazilian customer or 5 sign-ups, whichever comes first. Owned by FEE (i18n scaffolding) + SOE (translations).

**At any point — TL discretion:**

23. **Stop the bus**: If the live `hermes-setup` fleet starts having operational issues during the recreation, can we re-prioritize away from Phase 3 back to Phase 1/2 hardening? Plan assumes yes; default behavior is operator-plane reliability over customer-plane velocity.
