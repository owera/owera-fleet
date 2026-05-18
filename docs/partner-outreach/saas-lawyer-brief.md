# SaaS lawyer — engagement brief (Owera Software Ltda)

## Who we are

**Owera Software Ltda** — Brazilian SaaS company headquartered in **Macapá, Amapá** (CNPJ: TBD — operator to fill). Product: **Owera Agentic**, a managed agentic-work service. Customer plane (`api.owera.ai`) is live; first paying customer expected within 60 days. Founder + sole operator: Rodrigo Recio.

## What we need

Brazilian SaaS-experienced lawyer (Macapá-based **or** São Paulo-based with remote engagement) for **commercial + privacy** work. **Hard block:** without signed MSA + DPA + privacy policy, we cannot onboard the first paying customer (T20.1 in the founding plan).

### Deliverables (in priority order)

1. **LGPD-compliant privacy policy** (PT-BR + EN) referencing Owera Software Ltda as the **controller**. Must address: data classification, retention windows (≤ 30 days for right-to-erasure per T18.2), DPO designation (Rodrigo as DPO for V0), ANPD notification process for incidents.
2. **DPA template** (data processing agreement) to attach to MSA. Compliant with LGPD Art. 39-41 (processor obligations).
3. **SCC annex** (Standard Contractual Clauses) for **EU ↔ BR transfers**. Triggers as soon as the first EU customer onboards (expected in beta).
4. **MSA template** (Master Services Agreement) suitable for SMB → mid-market SaaS contracts. Should support:
   - Usage-billed and recurring-subscription SKUs (Owera Agentic has both)
   - USD billing under BR entity (international receipts)
   - 30-day post-termination data retention before purge
   - Liability cap at 12 months trailing fees (industry norm)
   - LGPD obligations cross-referenced from the DPA
5. **Terms of Service** for the public-facing dashboard (`app.owera.ai`).

### Out of scope (for V0, may expand later)

- IP / trademark (handled by separate IP counsel if/when needed)
- Equity / employment law (no employees yet; revisit at first hire)
- US-side contracts (Delaware C-corp, etc.) — not on the V0 roadmap
- Disputes / litigation

## Timeline

| Milestone | Target |
|---|---|
| Engagement letter signed | Within 2 weeks of first outreach |
| MSA + DPA + SCC annex first draft | Within 4 weeks of engagement |
| Privacy policy + ToS (PT-BR + EN) | Within 4 weeks of engagement |
| Final round + signature on Owera-side templates | Within 6 weeks of engagement |

**Hard deadline:** before T20.1 in the founding plan (first paying customer onboarding). Currently slipping into ~60 days from today.

## Budget envelope

Operator to fill. Reference points to ask candidates about:
- Fixed fee for deliverables 1-5 (one-shot package): BRL ___ to ___
- Per-contract review (when a customer redlines our MSA): BRL ___ / contract
- Ongoing retainer (advice + minor edits): BRL ___ / month — optional, not committed at engagement

## Selection criteria

Prefer:

1. **Prior LGPD work** for SaaS companies (asks the right Art. 18 / Art. 41 questions)
2. **Prior SaaS contract work** (MSA / DPA / SLA — not generic commercial law)
3. **Cross-border experience** specifically EU ↔ BR transfers (knows SCC mechanics)
4. Bilingual (PT-BR + EN); customers and contracts will be in both
5. Comfortable with founder-driven small companies; not a "must talk to your general counsel" mindset

Disqualifiers:

- Big-firm partner whose minimum engagement is 6-figure BRL
- No experience with software-as-a-service contracts (insists on physical-deliverable language)
- Cannot commit a calendar for deliverables 1-5

## Initial outreach list

Operator to fill — recommended starting points:

- OAB-AP (Ordem dos Advogados, Amapá) referral list — SaaS-specialist filter
- São Paulo SaaS-focused law firms with remote-first practice (e.g. \_\_\_, \_\_\_, \_\_\_)
- Stripe Brasil's partner-lawyer directory (if it exists; ask Stripe BR account team)
- Referrals from other Brazilian SaaS founders (operator network)

## Initial-meeting agenda (60 min)

1. Owera intro + product demo + data-flow diagram (10 min)
2. LGPD posture conversation (15 min — let them ask: what data, where, how long, who?)
3. Their proposed scope + pricing (15 min)
4. Sample documents (ask them for an anonymised MSA or DPA they've delivered) (10 min)
5. Next steps + tentative engagement letter date (10 min)

## Success metric

**Signed MSA + DPA + LGPD privacy policy in place, with a customer-facing template that the first paying customer signs without escalating to *their* counsel, on or before T20.1.**

---

## Cross-references

- `knowing-all-you-now-calm-leaf.md:915` — original requirement
- `knowing-all-you-now-calm-leaf.md:23, 27` — LGPD + SCC requirements
- `~/owera-cloud/api/internal/audit/audit.go` — WORM audit log (LGPD Art. 39 traceability evidence)
- `~/owera-cloud/api/cmd/erasure-worker/` — right-to-erasure pipeline (LGPD Art. 18 enforcement)
- `~/owera-cloud/compliance/policies/` — existing policy stubs (vendor-management, incident-response, etc.) the lawyer will need to read
- `~/owera-cloud/compliance/runbooks/customer-data-deletion.md` — operational realization of erasure
- `~/owera-fleet/docs/partner-outreach/tax-accountant-brief.md` — companion engagement (independent, can run in parallel)
