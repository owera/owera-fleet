# `remote/` — worker-side bash fragments

Files in this directory are bash fragments **scp'd to workers and executed
on stock macOS `/bin/bash` (Bash 3.2)**, not on the gateway. They are the
small surface where the bash-3.2 constraint still binds — everything else
in the fleet is the typed Go binary. Each fragment is self-contained
(no `source`d gateway libs), emits its structured telemetry as JSONL to
**stderr** in the canonical Owera schema
`{ts, node, phase, action, result, duration_ms, stderr_tail}` so the
gateway orchestrator (`internal/bootstrap`) can fold it into
`~/.hermes/logs/bootstrap.jsonl`, and must be **idempotent** — re-running
emits `no-change` for every already-satisfied action. `shellcheck -s
bash` must pass clean on every file here; CI enforces this.

## Bootstrap pipeline phases

The full pipeline is ten phases, run in order by
`fleetctl bootstrap-worker`. Each phase is one script in this directory.

| Phase | Script | Mutations | Inputs from gateway |
|------:|--------|-----------|---------------------|
| 00 | `phase00_brew_baseline.sh` | Homebrew + essentials (jq, coreutils, shellcheck, gh, wget, ripgrep) | none (Homebrew must already be admin-installed) |
| 01 | `phase01_verify_prereqs.sh` | none (read-only) | none |
| 02 | `phase02_create_user.sh` | creates `hermes` user (UID 502, /Users/hermes, com.apple.access_ssh) | none |
| 03 | `phase03_install_hermes.sh` | installs hermes at pinned version under `hermes`; seeds `~/.hermes/PINNED_VERSION` | `--pinned-version TAG` |
| 04 | `phase04_seed_config.sh` | extracts gateway config.yaml (+ skills, templates) into `~/.hermes/`; rewrites `tirith_path:` | tarball at `--config-bundle PATH` |
| 05 | `phase05_distribute_secrets.sh` | installs `.env.gpg` (mode 600) + `run-with-secrets.sh` (mode 700) | `.env.gpg` at `--env-gpg PATH`, wrapper at `--wrapper PATH` |
| 06 | `phase06_setup_dedicated_key.sh` | installs gateway-side ed25519 pubkey into `/Users/hermes/.ssh/authorized_keys` | `.pub` at `--pubkey PATH` |
| 07 | `phase07_install_tirith.sh` | best-effort `brew install tirith`, falls back to `hermes skill run hermes-container-security`, else defers to Hermes auto-fetch at runtime | none |
| 08 | `phase08_launchd_heartbeat.sh` | installs heartbeat LaunchDaemon at `/Library/LaunchDaemons/com.hermes.heartbeat.plist`, bootstraps it, waits up to 70s for first tick | rendered plist at `--plist PATH` |
| 09 | `phase09_verify.sh` | none (read-only acceptance gate); emits canonical `verify` JSONL event | `--pinned-version TAG` |

All phases accept `--node NAME` (override the JSONL `node` field) and
`--dry-run` (probe-only — phases that mutate report `skipped` for each
would-be mutation). Run a single phase with
`fleetctl bootstrap-worker --node hermes@host --phase phaseNN_<name>.sh`.
