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
