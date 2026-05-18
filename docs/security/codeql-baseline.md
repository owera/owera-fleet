# CodeQL baseline (owera-fleet)

> **Purpose:** day-one CodeQL findings get triaged here so the workflow stays green on `main`. Three buckets per alert: **fix**, **dismiss in-GitHub** with a reason, or **record below** as an accepted finding with rationale.

The CodeQL workflow lives at [`.github/workflows/codeql.yml`](../../.github/workflows/codeql.yml). It scans **Go only** (this is a single-language repo; `owera-cloud` has the Go + JavaScript matrix). Triggers: push to `main`, PRs against `main` or `wave-*`, weekly Monday 06:00 UTC cron.

## Accepted findings

| CodeQL rule | Path | Rationale | Reviewer | Date |
|---|---|---|---|---|

(Empty — populated as the workflow runs against `main` and a real finding surfaces.)

## Workflow

1. CodeQL alert fires on a PR or scheduled run.
2. Triage: is it real? Can it be fixed in-PR? If yes → fix it. If false-positive → dismiss in the GitHub Security tab with the dismissal reason filled in. If real-but-accepted (e.g. an intentional pattern we don't want to change) → add a row above and link the dismissal.
3. Don't leave the workflow red. A red CodeQL on `main` blocks every subsequent PR's CodeQL check from auto-passing.

## Cross-references

- Sister doc: [`owera-cloud/docs/security/codeql-baseline.md`](https://github.com/owera/owera-cloud/blob/main/docs/security/codeql-baseline.md)
- SOC 2 evidence: this baseline is one input to the Security TSC control "vulnerability management" — see `owera-cloud/compliance/audit-controls/tsc-security.md`.
