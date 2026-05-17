# owera-fleet

The **operator plane** for **Owera Agentic** — a typed Go runtime that provisions and operates a Hermes Agent fleet of macOS workers.

This repo replaces the earlier `hermes-setup` bash prototype with a single-binary dispatcher (`fleetctl`) plus a small shared library. The sister repo [`owera-cloud`](https://github.com/owera/owera-cloud) wraps this fleet with a public API, customer dashboard, and billing — together they ship the productized service.

## Status

🟢 **Phase 1 + Phase 2 primitives mostly shipped; Phase 2.5 customer-plane seam is live in production.** All twelve Phase 2 primitive packages are in `internal/` with matching `fleetctl` subcommands. The JSON-RPC server (`fleetctl serve`) runs under launchd on `claw3.local` and answers `fleet.SubmitJob`, `fleet.CancelTask`, `fleet.HealthSnapshot`, `fleet.LedgerTail` calls from the cloud plane over a Cloudflare Named Tunnel.

Outstanding before Phase 4 launch readiness: end-to-end scenario coverage per V0 SKU on the live fleet; cutover from the bash prototype's `STATE.md` to `fleetctl state --markdown`; staging Mac provisioned; on-call drill. See [`docs/roadmap.md`](docs/roadmap.md) for the full board and [`knowing-all-you-now-calm-leaf.md`](knowing-all-you-now-calm-leaf.md) for the master plan. Sister repo: [`owera-cloud`](https://github.com/owera/owera-cloud) (customer plane).

## Quickstart (planned)

```bash
# Install (once binaries ship to GitHub Releases):
curl -fsSL https://github.com/owera/owera-fleet/releases/latest/download/fleetctl-darwin-arm64.tar.gz | tar -xz

# Inspect fleet state
fleetctl state

# Bootstrap a new worker (idempotent; brew → Hermes → keys → LaunchAgents)
fleetctl bootstrap-worker hermes@new-worker.local

# Daily ops
fleetctl delegate --all --cmd "uname -a"
fleetctl health
fleetctl audit config
fleetctl test --tier=smoke
```

## Repo layout

```
cmd/fleetctl/      single-binary dispatcher; one file per subcommand under commands/
internal/          typed core: log, nodes, ssh, bootstrap, audit, launchd, scenarios, …
remote/            bash 3.2 fragments scp'd to workers (the only place bash lives)
templates/         launchd plists, markdown report skeletons
scenarios/         test data: smoke / usecase / readiness / e2e tiers
skills/            auto-generated SKILL.md manifests (one per command)
docs/              operation.md, plan.md, security.md, roadmap.md, archive/
```

## Development

```bash
# Toolchain (one-time)
brew install go shellcheck gitleaks golangci-lint

# Build + test
go build ./...
go test ./...

# Lint
golangci-lint run
shellcheck remote/*.sh
gitleaks detect --source=.
```

Go 1.22+ required. Bash 3.2 compatibility is enforced for files under `remote/` (the only place that runs on stock-macOS workers).

## License

Proprietary. Copyright © 2026 Owera Software Ltda. See [`LICENSE`](LICENSE) for terms. Public repo for transparency and trust signaling; not open source.
