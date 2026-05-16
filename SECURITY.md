# Security Policy

## Supported versions

`owera-fleet` is in early development. Only the latest tagged release is supported.

## Reporting a vulnerability

**Do not open a public GitHub issue for security-relevant findings.** Instead, email `security@owera.com` with:

- A description of the issue and the affected component.
- Steps to reproduce (a minimal failing case is ideal).
- Your assessment of impact and severity.
- Any suggested mitigation.

We'll acknowledge receipt within **2 business days (Brazil business hours, UTC−3)** and aim to publish a fix or mitigation within 30 days for high-severity issues. We do not run a bug bounty program at this time; we will acknowledge contributors in release notes if you wish.

## Scope

In scope:
- `cmd/fleetctl/` — the dispatcher binary.
- `internal/` — typed core libraries.
- `remote/` — bash fragments executed on worker nodes.
- LaunchAgent templates under `templates/launchd/`.

Out of scope (handled in the sister repo or vendor relationship):
- The customer-facing API and dashboard — those live in [`owera-cloud`](https://github.com/owera/owera-cloud).
- Third-party services (Stripe, Cloudflare, PagerDuty) — report to the vendor directly.

## Compliance posture

Owera Software Ltda is a Brazilian controller subject to **LGPD** (Lei Geral de Proteção de Dados). Customer data residency, deletion (right-to-erasure), and audit-log retention are documented in the privacy policy at [`https://owera.com/privacy`](https://owera.com/privacy) and in `owera-cloud/compliance/policies/`.

This repo also targets **SOC 2 Type 1** readiness (12-month horizon). Technical evidence — signed ledger, audit log, secret handling, signed config sync — is built progressively; control mapping lives in `owera-cloud/compliance/audit-controls/`.
