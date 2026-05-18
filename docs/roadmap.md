# owera-fleet — operator-plane roadmap

> **Audience: internal team + cloud-plane developers.** Sister doc: [`owera-cloud/docs/roadmap.md`](https://github.com/owera/owera-cloud/blob/main/docs/roadmap.md). Master plan that defined the green-field recreation: [`knowing-all-you-now-calm-leaf.md`](../knowing-all-you-now-calm-leaf.md). Forward view; source of truth for delivered state is the git log on `main`.

**Last updated:** 2026-05-17 (late evening, post-Wave-10-Track-A + V0 Stripe demo + public flip).

## What we're working on right now

**Phase 1, 2, 2.5, 3 + Track-A hardening sprint + first V0 Stripe demo all closed in the evening of 2026-05-17.** A live `campaign-swarm@v1` job submitted via `https://owera-agentic-api.fly.dev/v1/jobs` traversed the full chain (cloud queue → dispatcher → JSON-RPC over the tunnel → operator-plane SKURouter → ledger → cloud bill subscriber → outbox → reconciler → Stripe SDK) and resulted in a real test-mode Stripe InvoiceItem on `cus_UXImEhwCti1Aq6`. Both repos flipped to public; CodeQL + GHAS + free Actions all active.

| Thread | Status | Where |
|---|---|---|
| Operator plane is **live on `claw3.local`** serving the production cloud API over a Cloudflare Named Tunnel | ✅ Shipped | 4 LaunchAgents (`com.owera.fleetctl-serve`, `snapshot-publish`, `heartbeats-bridge`, `com.cloudflare.cloudflared`) running |
| Cloud reconciler wire-up (outbox flusher → Stripe + daily drift detector) | ✅ Shipped + deployed | Production boot log shows `reconciler=on (drift detector, daily)`; outbox flusher tick interval 1m; drift detector tick interval 24h |
| Phase-1 verification gate (T4.4 `audit config` + T12.1 `state --markdown` parity + T12.3 `bootstrap-worker` phases 1-9) | ✅ Shipped to `main` | PR #14 (audit config), #19 (bootstrap phases), #20 (state parity) |
| Phase-2 e2e scenarios — usecases 32/33/34 now exercise live primitives, not help-text | ✅ Shipped to `main` | PR #15 (swarm), #16 (cronjob+alert), #17 (markers CLI + usecase34 wiring) |
| **V0 end-to-end Stripe demo** — real Stripe InvoiceItem fired from a live `campaign-swarm@v1` job submitted via `/v1/jobs` | ✅ Demo'd 2026-05-18 ~00:03 UTC | Customer `cus_UXImEhwCti1Aq6` in Stripe test mode |
| **Track A hardening sprint** — long-running SKURouter (PR #26), tier selection (PR #25), V1 SKU stubs (PR #27), CodeQL workflow (PR #28) on fleet side | ✅ Merged | All 4 fleet Track-A PRs landed; CodeQL active on `main` with zero day-one findings |
| **Repos public** — `owera/owera-fleet` + `owera/owera-cloud` both flipped 2026-05-18 ~00:50 UTC | ✅ Operator-action complete | GHAS + unlimited free Actions + CodeQL all active on both |
| Cutover from `hermes-setup` bash prototype (C3) | 🚧 Gated on C1 staging-Mac verification of `fleetctl bootstrap-worker hermes@claw-staging.local` | Archive directory `docs/archive/` exists, empty; awaits the cutover commit |

## Phase status at a glance

| Phase | Scope | State |
|---|---|---|
| **Phase 1** — Foundation | Typed Go core (`internal/log`, `nodes`, `ssh`, `bootstrap`, `audit`, `launchd`, `scenarios`, `state`), `fleetctl` dispatcher with 28 subcommands | ✅ Code shipped to `main`. Verification gate items (T4.4 audit config, T12.1 state parity, T12.3 bootstrap-worker phases 00-09) all landed Wave-9. Live VM acceptance run still gated on `claw-staging.local` |
| **Phase 2** — Use-case primitives | `internal/{ledger, pairing, budget, secrets, configsync, hermesjobs, markers, orchestrator, alerting, metrics, skillsync, logaggregate}` + matching `commands/*.go` + `markers` CLI | ✅ All 12 primitives + 12 commands shipped. E2E scenarios usecase31/32/33/34 exercise live primitives (PR #15/#16/#17 Wave-9-A). |
| **Phase 2.5** — Customer-plane seam | `internal/rpc` JSON-RPC 2.0 server (`fleet.SubmitJob`, `fleet.CancelTask`, `fleet.HealthSnapshot`, `fleet.LedgerTail`); `cmd/fleetctl/commands/serve.go`; snapshot publisher + R2/S3 upload mode; heartbeats-bridge | ✅ Shipped. Cloud plane `apiserver` reaches this surface over the Cloudflare tunnel at `https://internal-rpc.owera.com` |
| **Phase 3** — Customer plane | Lives in `owera-cloud` | ✅ Engineering complete. See [cloud roadmap](https://github.com/owera/owera-cloud/blob/main/docs/roadmap.md). Reconciler live in prod (Wave-9 PR #37). |
| **Phase 4** — Launch readiness | Staging Mac, on-call rotation, beta-1 design partners | ⏳ Operator-action gated: `claw-staging.local` hardware, PagerDuty account, Stripe live-mode, repo-public flip, BR tax accountant, BR SaaS lawyer |

## Phase 1 — open items

The Phase 1 build per the founding plan is "1) typed core, 2) generated docs over hand-edited, 3) bootstrap idempotent, 4) LaunchAgents installed = loaded, 5) tests as data, 6) SKILL.md generated." Items still pending:

| Item | Status | Notes |
|---|---|---|
| `fleetctl bootstrap-worker` proven on a fresh macOS VM | 🚧 Code shipped for phases 00-09 (PR #19); live VM run gated on `claw-staging.local` Mac procurement | Founding-plan verification step 5 |
| `fleetctl state --markdown` structural parity with `hermes-setup/STATE.md` | ✅ Shipped (PR #20); diff returns only timestamp/PID/narrative lines | Founding-plan migration step 4; cutover gate |
| `fleetctl audit config` drift table matches the bash-era hand-maintained version | ✅ Shipped (PR #14); reproduces SECURITY_NOTES.md table byte-for-byte | Founding-plan verification step 7 |
| `fleetctl gen-skills --check` wired into CI | ✅ CI gate active in `.github/workflows/ci.yml`; 28 skills/SKILL.md tracked | Founding-plan principle 7 |
| Cross-compile release workflow (`darwin/arm64` + `darwin/amd64`) | ✅ `.github/workflows/release.yml` ready; fires on `v*` tag push | Founding-plan tradeoff §1 |

## Phase 2 — primitive coverage

All 12 primitives + matching CLI commands shipped. E2E scenarios for V0 SKUs now exercise live primitives (Wave-9-A landed PR #15/#16/#17).

| Component | Built | E2E scenario |
|---|---|---|
| `internal/ledger` | ✅ | usecase31 (pair + ledger), usecase34 (resume via markers) |
| `internal/pairing` + `internal/budget` + `commands/pair.go` | ✅ | usecase31 (pair create/budget/show/revoke round-trip) |
| `internal/secrets` + `commands/run-with-secrets.go` | ✅ | usecase33 (env-leak prevention + opt-in injection) |
| `internal/configsync` + `commands/config-sync.go` | ✅ | Unit tests; e2e covered indirectly via bootstrap-worker phase04 |
| `internal/hermesjobs` + `commands/cronjob.go` | ✅ | usecase33 (cronjob install --dry-run round-trip) |
| `internal/markers` + `commands/markers.go` *(new in Wave-9 PR #17)* | ✅ | usecase34 (hash/verify/list/invalidate end-to-end on ETL pipeline) |
| `internal/orchestrator` + `commands/swarm.go` | ✅ | usecase32 (live fan-out hermes@claw1+claw2 + ledger merge verify) |
| `internal/alerting` + `commands/alert.go` | ✅ | usecase33 (ntfy backend fire) |
| `internal/metrics` + `commands/metrics-cmd.go` | ✅ | usecase34 (scrape + ledger verify) |
| `internal/skillsync` + `commands/skill-sync.go` | ✅ | Unit tests; signed-pull scenario remains TBD pending real signing infra |
| `internal/logaggregate` + `commands/log-aggregate.go` | ✅ | Daemon installed via `setup-agent` |

**Phase 2 → Phase 4 gating scenarios** (founding-plan §"Phase 2 build order" step 25):

- ✅ `scenarios/usecase31_native_app.yaml` — pair + ledger round-trip (pre-Wave-9; real)
- ✅ `scenarios/usecase32_marketing_swarm.yaml` — live swarm fan-out + ledger merge (Wave-9 PR #15)
- ✅ `scenarios/usecase33_ticket_triage.yaml` — cronjob round-trip + run-with-secrets + alert fire (Wave-9 PR #16)
- ✅ `scenarios/usecase34_etl.yaml` — markers CLI + cronjob + parallel delegate + ledger (Wave-9 PR #17)

C2 e2e gauntlet (`fleetctl test --tier=e2e` against live fleet) is the next executable verification; pending `claw-staging.local` provisioning for the bootstrap-worker side, but the scenario suite itself is ready.

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
| 2026-05-17 (evening) | Claude (under Rodrigo) | Wave-9-A + B1 close-out. 7 owera-fleet PRs merged (#14 audit config, #15 swarm e2e, #16 cronjob+alert e2e, #17 markers CLI, #18 drift cleanup, #19 bootstrap phases 1-9, #20 state parity) + 2 utility PRs (#13 drift fix already merged AM; #18 same-evening). Phase-1 verification gate engineering all green; Phase-2 e2e scenario coverage complete. Remaining holdouts (C1 staging Mac, C2 live gauntlet, C3 cutover) are operator-action-bound. |
| 2026-05-17 (late evening) | Claude (under Rodrigo) | WS-A + V0 Stripe demo + Track-A hardening. Fleet PRs: #23 (WS-A stub routers — first V0 SKUs registered), #24 (campaign-swarm tier-letter meter convention fix), #25 (H2 tier selection from inputs), #26 (H1 long-running SKURouter contract), #27 (H4 V1 operator stubs research-brief/code-audit), #28 (CodeQL workflow). Plus 5 cloud-side PRs in same session. **Real Stripe InvoiceItem fired in production 2026-05-18 ~00:03 UTC** from a live `campaign-swarm@v1` submission — founding-plan verification step 10 demonstrably closed. Both repos flipped public ~00:50 UTC. CodeQL active on `owera-fleet/main` with zero day-one findings. Engineering critical path through Phase 3 is fully closed; only operator-action items remain (claw-staging.local, Stripe live mode, PagerDuty, BR tax/legal, design partner #1). |
