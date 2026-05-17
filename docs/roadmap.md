# owera-fleet — operator-plane roadmap

> **Audience: internal team + cloud-plane developers.** Sister doc: [`owera-cloud/docs/roadmap.md`](https://github.com/owera/owera-cloud/blob/main/docs/roadmap.md). Master plan that defined the green-field recreation: [`knowing-all-you-now-calm-leaf.md`](../knowing-all-you-now-calm-leaf.md). Forward view; source of truth for delivered state is the git log on `main`.

**Last updated:** 2026-05-17 (PM).

## What we're working on right now

| Thread | Status | Where |
|---|---|---|
| Operator plane is **live on `claw3.local`** serving the production cloud API over a Cloudflare Named Tunnel | ✅ Shipped | 4 LaunchAgents (`com.owera.fleetctl-serve`, `snapshot-publish`, `heartbeats-bridge`, `com.cloudflare.cloudflared`) running |
| End-to-end smoke of a V0 SKU through cloud → tunnel → operator → ledger → Stripe | 🚧 Gated on operator action (Stripe test-mode key already wired; missing piece is rotating/locating `OWERA_ADMIN_TOKEN` to mint the first API key) | Next executable step |
| Phase 2 verification: do the e2e scenarios in `scenarios/` exercise every primitive end-to-end? | 🔍 Audit pending | `scenarios/usecase3{1,2,3,4}.yaml` |
| Cutover from `hermes-setup` bash prototype | 🔄 In progress; `fleetctl serve` + `snapshot-publish` + `heartbeats-bridge` already supersede the bash equivalents | The bash repo will be archived under `docs/archive/hermes-setup-<date>/` once `fleetctl state --markdown` matches the bash STATE.md and `fleetctl bootstrap-worker` is proven on a fresh node |

## Phase status at a glance

| Phase | Scope | State |
|---|---|---|
| **Phase 1** — Foundation | Typed Go core (`internal/log`, `nodes`, `ssh`, `bootstrap`, `audit`, `launchd`, `scenarios`, `state`), `fleetctl` dispatcher with 20+ subcommands | ✅ Mostly shipped to `main`; outstanding items in §"Phase 1 — open items" |
| **Phase 2** — Use-case primitives | `internal/{ledger, pairing, budget, secrets, configsync, hermesjobs, markers, orchestrator, alerting, metrics, skillsync, logaggregate}` + matching `commands/*.go` | ✅ All 12 primitives + 11 of 12 commands shipped (commands/ as of 2026-05-17). E2E scenarios per use case still to be authored against the live primitives |
| **Phase 2.5** — Customer-plane seam | `internal/rpc` JSON-RPC 2.0 server (`fleet.SubmitJob`, `fleet.CancelTask`, `fleet.HealthSnapshot`, `fleet.LedgerTail`); `cmd/fleetctl/commands/serve.go`; snapshot publisher + R2/S3 upload mode; heartbeats-bridge | ✅ Shipped. Cloud plane `apiserver` reaches this surface over the Cloudflare tunnel at `https://internal-rpc.owera.com` |
| **Phase 3** — Customer plane | Lives in `owera-cloud` | ⏩ See [cloud roadmap](https://github.com/owera/owera-cloud/blob/main/docs/roadmap.md) |
| **Phase 4** — Launch readiness | Staging Mac, on-call rotation, beta-1 design partners | ⏳ Planned; gated on V0 e2e smoke + Stripe live mode + repo-public flip |

## Phase 1 — open items

The Phase 1 build per the founding plan is "1) typed core, 2) generated docs over hand-edited, 3) bootstrap idempotent, 4) LaunchAgents installed = loaded, 5) tests as data, 6) SKILL.md generated." Items still pending:

| Item | Status | Notes |
|---|---|---|
| `fleetctl bootstrap-worker` proven on a fresh macOS VM | 🚧 Built in code; no end-to-end run against a clean node yet | Founding-plan verification step 5 |
| `fleetctl state --markdown` byte-for-byte matches `hermes-setup/STATE.md` | 🚧 Open | Founding-plan migration step 4; cutover gate |
| `fleetctl audit config` drift table matches the bash-era hand-maintained version | 🚧 Open | Founding-plan verification step 7 |
| `fleetctl gen-skills --check` wired into CI | ✅ Generated `skills/<name>/SKILL.md` committed; CI drift check still TBD | Founding-plan principle 7 |
| Cross-compile release workflow (`darwin/arm64` + `darwin/amd64`) | 🚧 Open | `.github/workflows/release.yml` |

## Phase 2 — primitive coverage

Built and exercised at unit-test level. Outstanding: end-to-end scenario coverage that actually runs each SKU's full primitive chain on the live fleet.

| Component | Built | E2E scenario landed |
|---|---|---|
| `internal/ledger` | ✅ | Partial (used by `fleet.LedgerTail`; SKU-shaped replay scenario TBD) |
| `internal/pairing` + `internal/budget` + `commands/pair.go` | ✅ | TBD |
| `internal/secrets` + `commands/run-with-secrets.go` | ✅ | TBD |
| `internal/configsync` + `commands/config-sync.go` | ✅ | TBD (minisign integration verified in unit tests) |
| `internal/hermesjobs` + `commands/cronjob.go` | ✅ | TBD |
| `internal/markers` | ✅ | TBD |
| `internal/orchestrator` + `commands/swarm.go` | ✅ | TBD |
| `internal/alerting` + `commands/alert.go` | ✅ | TBD (PagerDuty + OpsGenie + ntfy backends) |
| `internal/metrics` + `commands/metrics-cmd.go` | ✅ | TBD (Prometheus exposition) |
| `internal/skillsync` + `commands/skill-sync.go` | ✅ | TBD |
| `internal/logaggregate` + `commands/log-aggregate.go` | ✅ | TBD |

**Phase 2 → Phase 4 gating scenarios** (founding-plan §"Phase 2 build order" step 25):

- `scenarios/usecase31_native_app.yaml` — `pair → delegate → ledger → swarm → gh pr create → alert` for `app-build`/`xcode-ci`
- `scenarios/usecase32_marketing_swarm.yaml` — multi-leaf swarm + ledger merge + idempotency-key compensation for `campaign-swarm`
- `scenarios/usecase33_ticket_triage.yaml` — `cronjob` (hermes-managed, not launchd) + `secrets` (stdin) + `alert` for `triage-watch`
- `scenarios/usecase34_etl.yaml` — `cronjob` + parallel `delegate` + `markers` + `ledger` resume for `etl-flow`

These are the "do the primitives actually compose into a sellable SKU end-to-end" test. Authoring them and getting them green is the next chunk of Phase 2 work.

## Phase 2.5 — customer-plane seam (what makes the cloud plane work)

Shipped:

- **JSON-RPC 2.0 server** in `internal/rpc` exposing `fleet.SubmitJob`, `fleet.CancelTask`, `fleet.HealthSnapshot`, `fleet.LedgerTail`. Bound to `127.0.0.1:9091` on the gateway.
- **`fleetctl serve`** subcommand that runs the server under launchd as `com.owera.fleetctl-serve`.
- **Snapshot publisher** (`fleetctl snapshot-publish`) writes `~/.hermes/status/snapshot.json` every 30 s; `--http-put` mode supports direct R2/S3 upload for a future public `status.owera.ai` page.
- **Heartbeats bridge** (`fleetctl heartbeats-bridge`) SSH-polls workers and refreshes `~/.hermes/heartbeats/<host>.json` so `fleet.HealthSnapshot` reports workers as `ok=true`.
- **launchd templates** for all four agents (`com.owera.*` + `com.cloudflare.cloudflared`); `fleetctl setup-agent` installs and loads them.

What's open:

- Public **status page** generator that consumes the published snapshot. Lives in `owera-cloud/status/`.
- **Worker self-update Tirith policy** for the day a worker runs its own agent loop (currently latent — workers are SSH-only).
- **Shared write target for swarm fan-out** (founding-plan operator-horizon item; gates `etl-flow` and any genuine multi-node concurrent-append SKU).
- **Apple-toolchain hardening** (signed-build provenance, provisioning-profile rotation) — gates V3 `xcode-ci`/`app-build`.

## Phase 4 — launch readiness

Gated on:

1. V0 end-to-end smoke green (1 SKU through the full chain on test-mode Stripe).
2. Stripe test-mode → live mode cutover (operator action; account cleanup on Owera Fleet account).
3. `owera-cloud` repo visibility flip (private → public per founding-plan §"Public-repo discipline").
4. `owera-fleet` repo visibility flip (already public; double-check `gitleaks` clean before the cloud flip).
5. **Staging Mac (`claw-staging.local`)** provisioned and bootstrapped so changes can be tested without touching the production fleet.
6. **On-call drill** per founding-plan verification step 15 — PagerDuty fires within 2 min, runbook followed, post-mortem authored within 48 h.

## Deferred / out of scope

Items deliberately not in the current build. Re-evaluate at the 18-month mark or when a corresponding signal arrives.

| Item | Why deferred |
|---|---|
| Linux workers (any non-Apple arch) | Mac fleet is the unique advantage; adding Linux dilutes the differentiator without a customer asking |
| `nodes.txt` dispatcher beyond round-robin | Operator-side hygiene; gates `n_workers ≥ 4` with tag-routing |
| Per-target SSH key split | Operator key sprawl is documented and accepted; revisit if it ever feels off |
| Warm-standby gateway Mac | Gated on an RTO commitment with dollars attached |
| Hermes-on-worker (worker runs its own agent loop) | Layer-2 P7 gap from hermes-setup era; latent until a use case demands it |

## hermes-setup phase-out

The original `~/hermes-setup` bash workspace is being phased out. The cutover sequence:

1. **Now:** Both repos run in parallel on `claw3.local`. The operator-plane LaunchAgents are `com.owera.*`; the legacy bash hermes agents (`com.hermes.backup`, `com.hermes.watchdog`, `com.hermes.backup-worker`) still run for hermes-side backup + monitoring of the underlying Hermes Agent v0.13.0 install.
2. **Soon:** Once `fleetctl state --markdown` matches the bash `STATE.md` and `fleetctl bootstrap-worker` is proven on a fresh node, replace the bash backup/watchdog/logrotate agents with `fleetctl setup-agent` equivalents.
3. **Then:** Snapshot `~/hermes-setup/` into `owera-fleet/docs/archive/hermes-setup-<date>/`, leave the github.com/rrecio/hermes-setup repo as a single-line README pointing here ("Superseded by owera-fleet").
4. **End state:** `hermes-setup/` directory is read-only history; all operator-plane work happens in this repo.

## Change log

| Date | Author | Change |
|---|---|---|
| 2026-05-17 (PM) | Claude (under Rodrigo) | Initial publication. Captures Phase 1 + Phase 2 primitive coverage, Phase 2.5 customer-plane seam status, Phase 4 gating, hermes-setup phase-out sequence. |
