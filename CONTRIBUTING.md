# Contributing

This is a public repository operated by Owera Software Ltda. Issues are welcome; PRs are accepted at maintainer discretion.

## Before opening an issue

- For **bug reports**, include: `fleetctl` version, the JSONL log line that surfaced the issue, OS version. Don't paste secrets — redact them first.
- For **feature requests**, frame the customer outcome before the implementation. SKU-level requests should land via the `internal/catalog` template (see [`docs/sku-template.md`](docs/sku-template.md) once published) rather than free-form issues.
- For **security issues**, see [`SECURITY.md`](SECURITY.md) — disclose privately via email, not via a public issue.

## Before opening a PR

- Discuss in an issue first for anything beyond a small fix.
- Follow the existing voice: operational, command-oriented, table-heavy. No fluff in comments.
- Match the JSONL log schema and idempotency conventions documented in [`CLAUDE.md`](CLAUDE.md).
- CI must be green: `go test ./...`, `golangci-lint run`, `shellcheck remote/*.sh`, `gitleaks detect`, skill-manifest drift check.
- Each PR is small and self-contained. Multi-workstream changes are flagged for review.
- New SKUs: one PR per SKU (`internal/catalog/<sku>.go` + scenario + docs). The CI gate enforces "catalog-only" PR scope.

## Code of conduct

Be respectful. Owera is a small team; we expect collaborators to be likewise.
