# Tax accountant — engagement brief (Owera Software Ltda)

## Who we are

**Owera Software Ltda** — Brazilian SaaS company headquartered in **Macapá, Amapá** (CNPJ: TBD — operator to fill). Product: **Owera Agentic**, a managed agentic-work service sold to international and Brazilian customers as a catalogue of usage-billed and subscription SKUs. Founder + sole operator: Rodrigo Recio. Pre-revenue (V0 platform live; first paid customer expected within 60 days of engagement).

## What we need

Tax accountant on retainer, fluent in **Brazilian SaaS taxation** with experience in international receipts (USD billing under a BR entity). **Block:** we cannot publish prices in `docs/pricing.md` until this engagement closes, because prices must reflect net-after-tax economics. Without this, we set prices and discover margin is wrong after the first invoice.

### Deliverables (in priority order)

1. **ISS classification for AI/SaaS** in Macapá. Confirm the municipal rate (heard ~2-5 %; need authoritative number). Confirm whether agentic-work fits "software exports" carve-out when invoicing international customers.
2. **PIS/COFINS treatment** for the same revenue streams (cumulative vs non-cumulative regime; impact on margin).
3. **Net-after-tax pricing math** for the V0 SKU catalogue. The catalogue has 5 entries:
   - `triage-watch:base` ($499/mo recurring) + `triage-watch:ticket` ($2/ticket metered overage)
   - `campaign-swarm` 3 tiers ($499 / $999 / $1,999 one-time)
4. **Stripe Brazil onboarding guidance**: KYC docs needed for CNPJ → Stripe Brasil; budget the 2-4 week typical lead time for boleto/Pix support. (USD billing under the BR entity ships immediately; BRL needs Stripe Brasil.)
5. **Quarterly compliance**: file the routine submissions; provide a single-page year-end tax summary for the founder's personal IRPF.

### Out of scope (for V0, may expand later)

- Multi-currency hedging (we transact USD, hold partial USD via Stripe payouts, convert as needed)
- US tax setup (Delaware C-corp, etc.) — not on the roadmap; revisit if a US enterprise customer demands it
- Brazil M&A or fundraising tax structuring — defer until a real round is on the table

## Timeline

| Milestone | Target |
|---|---|
| Engagement letter signed | Within 2 weeks of first outreach |
| ISS + PIS/COFINS opinion delivered | Within 2 weeks of engagement |
| Net-after-tax pricing math delivered | Within 3 weeks of engagement |
| First quarterly filing | Per Brazilian calendar |

**Hard deadline:** before T16.1 in the founding plan (Stripe products + prices finalized) — currently slipping into ~30 days from today.

## Budget envelope

Operator to fill. Reference points to ask candidates about:
- Initial setup + opinion deliverables 1-4: BRL ___ to ___ (lump sum or hourly?)
- Quarterly retainer: BRL ___ / month
- Year-end work: separate or included?

## Selection criteria

Prefer:

1. Prior **AI/SaaS clients in Brazil** (asks the right questions; understands "software exports" carve-out)
2. Bilingual (PT-BR + EN) — many of our suppliers and customers are English-speaking
3. Macapá-based **OR** São Paulo-based with comfortable remote engagement model
4. Comfortable with founder-driven small companies (not a Big-4 alumnus who needs a CFO to talk to)
5. Available within typical Brazilian business hours; weekly 30-min sync acceptable

Disqualifiers:

- Cannot handle international USD receipts under a BR entity
- Wants a 6-month onboarding before producing first deliverable
- Refuses to commit price for deliverables 1-4 (lump-sum or capped hourly only)

## Initial outreach list

Operator to fill — recommended starting points:

- Macapá CRC (Conselho Regional de Contabilidade) referral list
- Referrals from other Macapá / Amapá SaaS founders (operator network)
- São Paulo SaaS-tax specialist firms with remote-first practice (e.g. \_\_\_, \_\_\_, \_\_\_)
- Stripe Brasil's partner-accountant directory (if it exists; ask Stripe BR account team)

## Initial-meeting agenda (60 min)

1. Owera intro + product demo (10 min)
2. Tax-classification questions per Deliverables 1-2 (20 min — let them ask)
3. Their proposed scope + pricing (15 min)
4. Communication cadence + tooling (5 min)
5. Next steps + tentative engagement letter date (10 min)

## Success metric

**Pricing math in `docs/pricing.md` is locked, with net-after-tax columns, before the first Stripe live-mode invoice is sent** (per the T16.1 dependency in `knowing-all-you-now-calm-leaf.md:927`).

---

## Cross-references

- `knowing-all-you-now-calm-leaf.md:24` — tax-rate baseline assumption
- `knowing-all-you-now-calm-leaf.md:927-929` — Brazil-specific blockers list
- `~/owera-cloud/api/internal/billing/stripe_ids.go` — current SKU catalogue
- `~/owera-cloud/infra/runbooks/stripe-live-cutover.md` — live-mode cutover (blocks on this engagement)
