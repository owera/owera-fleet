# Verification (WS-12 Phase-1 Gate)

The verification gate is the static-and-self-tests half of the WS-12 Quality-Verification-Evidence runbook. It runs on every PR (no SSH, no real hardware, no remote nodes) and decides whether the Phase-1 + Phase-2 build is shippable.

The live-fleet half (real workers, real Hermes, real customer-plane traffic) is **not** in this repo — it lives in `hermes-setup` as the `fleet-usecase-*` skill family. Run that runbook separately before promoting a release tag.

## Command

```
fleetctl verify             # text table, exits non-zero on any required failure
fleetctl verify --json      # machine-readable report
fleetctl verify --check ID  # run a subset; repeatable
fleetctl verify --repo PATH # override repo root (default: walk up from cwd to go.mod)
```

Each invocation appends one JSONL summary event to `~/.hermes/logs/verify.jsonl` with `phase=verify, action=gate, result=ok|error`. The text/JSON report is what an operator reads; the JSONL line is what the audit roll-up consumes.

## Checks

| ID | Name | What it does |
|---|---|---|
| `go_build` | Build succeeds | `go build ./...` from the repo root |
| `go_vet` | Vet clean | `go vet ./...` |
| `go_test` | Tests pass | `go test -race ./...` |
| `skills_drift` | Skill manifests in sync | `fleetctl gen-skills --check` exits 0 |
| `ledger_self` | Ledger round-trips | Temp-dir ledger: Open, Append, Read+Verify |
| `markers_self` | Markers round-trip | NewStore, Write, IsFresh(match)=true, IsFresh(diff)=false |
| `pairing_self` | Pairing round-trip | Create → Authenticate(token) → Revoke → Authenticate fails |
| `budget_self` | Budget enforces caps | SetLimits(N=2), Consume×2 OK, Consume×3 → `ErrRateLimitExceeded` |
| `configsync_self` | Configsync verifies | Genkey, PackDir, Verify OK; wrong-key Verify → `ErrBadSignature` |
| `metrics_self` | Metrics render | New(tmp logdir+1 line), Collect, Render → output starts with `# HELP` |

Checks run concurrently with a semaphore of 4. Each is also a "is this primitive integrated end-to-end?" smoke test: failure of `ledger_self`, for example, means the build compiles but the ledger package can't actually persist a signed entry in a fresh directory — that's a P0 ship-blocker.

The four shell-out checks (`go_*`, `skills_drift`) use `exec.CommandContext` with `cwd` pinned to the repo root resolved via `--repo` or by walking up from cwd looking for `go.mod`. If the shell-out command fails, the last 2 KiB of combined stdout/stderr is captured into `evidence`.

To regenerate this table after adding a check, list the new `Check` in `internal/verify/verify.go::DefaultChecks` and update the rows above by hand. There is no `--list` flag yet.

## Interpreting a failure

The text table renders one row per check. The summary line is `PASS` or `FAIL`, followed by counts and elapsed time. Exit code is 0 if `OK` (every **required** check passed) and 1 otherwise.

| Failed check | Where to look first |
|---|---|
| `go_build` | Compile error somewhere — read the captured stderr in `evidence`. |
| `go_vet` | Lint or shadowing — same. |
| `go_test` | One or more test failures; rerun the failing package directly for full output. |
| `skills_drift` | A subcommand's `Skill()` was edited without rerunning `fleetctl gen-skills`. Run it. |
| `ledger_self` / `markers_self` / `pairing_self` / `budget_self` / `configsync_self` / `metrics_self` | The primitive itself is broken. Run the package's `go test -race ./internal/<pkg>` for the full reproduction. The self-test only exercises the happy path of each primitive; a unit-test suite failure is usually a clearer signal. |

Checks declared `Optional` (currently none in the default set) surface in the report as `fail*` and do **not** flip `OK` to false. The annotation prefix `fail*` in the text table is the only operator-visible difference.

## When to bypass

There is no bypass. CI runs `fleetctl verify --json` on every PR and uploads the report as a build artifact. A failing gate fails the PR.

For local development, run `fleetctl verify --check go_test` to skip the (multi-second) `gen-skills` step while iterating, then `fleetctl verify` (no filter) before pushing.

## Pointer to the live-fleet QVE

The runbook for the live half lives in `hermes-setup`:

- `scripts/fleet-usecase-tests.sh` (Layer 1)
- `scripts/fleet-readiness-probes.sh` (Layer 2)
- `scripts/fleet-usecase-demos.sh` (Layer 3)

Those scripts exercise real workers over SSH, run actual Hermes delegations, and validate Phase-1 use cases end-to-end. They are deliberately not in this repo because they require the gateway's `~/.hermes/` state and a populated `nodes.txt`.
