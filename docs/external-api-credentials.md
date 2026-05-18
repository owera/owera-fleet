# External-API credentials for WS-B / WS-C SKUs

The `triage-watch@v1` (WS-B) and `campaign-swarm@v1` (WS-C) SKU routers depend on per-tenant external credentials resolved via `Secrets.GetForTenant(tenantID, "<service>")` (see `docs/sku-execution-spec.md:88-135`). This playbook is the operator-side procurement steps: which account to create, which scopes to grant, where to drop the credential value so the resolver finds it.

**Owner:** operator on behalf of each tenant (Rodrigo for Owera-internal smoke; tenant-of-record for live tenants).
**Estimated time:** ~10 minutes per service, modulo upstream signup friction (LinkedIn Marketing-tier approval can take days).

The order below is the order in which to procure them — the cheapest/fastest first so the design-partner smoke can fire on day 1 with whichever creds arrive first.

---

## 1. SendGrid (campaign-swarm@v1 — email channel)

Lowest friction. Free tier ships ≤ 100 emails/day; sufficient for smoke.

1. Sign up at https://signup.sendgrid.com/ — free tier (no credit card).
2. **Verify a sender identity** — Settings → Sender Authentication. Either a single sender (faster) or a verified domain (required for production). For Owera smoke: single sender `noreply@owera.ai`.
3. Settings → API Keys → Create API Key:
   - Name: `owera-fleet-<env>` (e.g. `owera-fleet-prod`).
   - Permissions: **Restricted Access** → grant only `Mail Send` (`mail.send`).
   - Copy the key once; it's shown only at creation.
4. Smoke test before handing to the fleet:
   ```bash
   curl --request POST \
     --url https://api.sendgrid.com/v3/mail/send \
     --header "Authorization: Bearer SG.xxxxx" \
     --header "Content-Type: application/json" \
     --data '{"personalizations":[{"to":[{"email":"rodrigo+test@owera.ai"}]}],"from":{"email":"noreply@owera.ai"},"subject":"smoke","content":[{"type":"text/plain","value":"smoke"}]}'
   # expect: HTTP 202, message in inbox within 30 s
   ```
5. Store under `Secrets.GetForTenant(tenantID, "sendgrid")`. For .env.gpg flow: `SENDGRID_API_KEY=SG.xxxxx` in the per-tenant env block.

---

## 2. Zendesk (triage-watch@v1)

Sandbox-friendly. Free trial available; existing Zendesk accounts have a Sandbox subdomain.

1. **Sandbox account** (recommended for smoke): Zendesk Admin Center → Account → Sandbox → Create. Sandbox subdomain `<acme>-sandbox.zendesk.com`.
2. Admin Center → Apps and integrations → APIs → Zendesk API → enable Token access.
3. **Generate API token**: "Add API token" → label `owera-fleet-triage-watch`. Copy once.
4. Scopes are token-wide (Zendesk doesn't offer per-token scopes); however, you authenticate as `<email>/token:<token>` with the API user's own role. Best practice: create a dedicated "service" user with role **Light Agent** restricted to view-only on the queues triage-watch monitors.
5. Smoke test:
   ```bash
   curl -u "svc-owera@example.com/token:xxxxxxxx" \
     "https://acme-sandbox.zendesk.com/api/v2/tickets.json?per_page=1"
   # expect: HTTP 200 + JSON with one ticket
   ```
6. Store under `Secrets.GetForTenant(tenantID, "zendesk")`. Composite shape:
   ```
   ZENDESK_SUBDOMAIN=acme-sandbox
   ZENDESK_EMAIL=svc-owera@example.com
   ZENDESK_API_TOKEN=xxxxxxxx
   ```

---

## 3. Twitter / X API (campaign-swarm@v1 — twitter channel)

**Highest friction** since the X policy churn — Basic tier is $200/mo for `tweet.write`. For smoke, the Free tier supports posting via the "App-only" path with rate caps that are sufficient.

1. https://developer.x.com/en/portal/dashboard — sign in with the brand X account (e.g. `@oweradotai`).
2. Create a Project → App. Note: the App is the OAuth client; one project can hold multiple apps (dev/prod/staging).
3. App settings → User authentication settings → enable OAuth 2.0 → request scopes:
   - `tweet.read`
   - `tweet.write`
   - `users.read`
4. Generate **Client ID + Client Secret** (under Keys and tokens). For server-to-server posting on behalf of the brand account, also generate the **OAuth 1.0a User Context** access tokens — those are the long-lived ones suitable for server use.
5. Smoke test:
   ```bash
   # Using OAuth 1.0a user-context (server posting as the brand account):
   curl --request POST 'https://api.x.com/2/tweets' \
     --header 'Authorization: OAuth oauth_consumer_key="…",oauth_signature_method="HMAC-SHA1",…' \
     --header 'Content-Type: application/json' \
     --data '{"text":"smoke"}'
   # then delete it via DELETE /2/tweets/<id>
   ```
6. Store under `Secrets.GetForTenant(tenantID, "twitter")`. Composite shape:
   ```
   TWITTER_API_KEY=...           # OAuth 1.0a consumer key
   TWITTER_API_SECRET=...        # OAuth 1.0a consumer secret
   TWITTER_ACCESS_TOKEN=...      # user context access token
   TWITTER_ACCESS_TOKEN_SECRET=...
   ```

---

## 4. LinkedIn (campaign-swarm@v1 — linkedin channel)

**Longest lead time** — Marketing Developer Platform tier is approval-gated and can take 5-10 business days. Request it as soon as the design partner is identified; do not block other procurement on it.

1. https://www.linkedin.com/developers/ — sign in with the brand account, create a new app.
2. Associate the app with the brand's LinkedIn **Page** (required for posting on behalf of the page).
3. App settings → Products → request **"Share on LinkedIn"** (immediate) AND **"Marketing Developer Platform"** (requires approval form; describe Owera as agentic post-scheduling for SMB marketing).
4. While waiting for approval, the "Share on LinkedIn" scope `w_member_social` covers posting on behalf of the **authenticating member** (not the page) — enough for a single-user smoke.
5. Generate OAuth 2.0 client ID + secret. Walk the auth flow once to obtain a long-lived access token (60 days; refresh-token cadence is the LinkedIn-standard 365 d).
6. Smoke test:
   ```bash
   curl -X POST https://api.linkedin.com/v2/ugcPosts \
     -H "Authorization: Bearer <access_token>" \
     -H "Content-Type: application/json" \
     -H "X-Restli-Protocol-Version: 2.0.0" \
     -d '{"author":"urn:li:person:<member-urn>","lifecycleState":"PUBLISHED","specificContent":{"com.linkedin.ugc.ShareContent":{"shareCommentary":{"text":"smoke"},"shareMediaCategory":"NONE"}},"visibility":{"com.linkedin.ugc.MemberNetworkVisibility":"CONNECTIONS"}}'
   # then DELETE /v2/ugcPosts/<urn-encoded-id>
   ```
7. Store under `Secrets.GetForTenant(tenantID, "linkedin")`. Composite shape:
   ```
   LINKEDIN_CLIENT_ID=...
   LINKEDIN_CLIENT_SECRET=...
   LINKEDIN_ACCESS_TOKEN=...     # long-lived; refresh cadence below
   LINKEDIN_REFRESH_TOKEN=...
   LINKEDIN_MEMBER_URN=urn:li:person:...
   ```

---

## Per-tenant credential storage

The `Secrets.Resolver` interface referenced in `sku-execution-spec.md` is the contract; the live backing today is `.env.gpg` per `internal/secrets/secrets.go:6`. For each tenant `tnt-<id>`:

1. Decrypt the operator-side vault entry: `gpg --decrypt vault/<tenant>.env.gpg > /tmp/<tenant>.env` (chmod 600).
2. Append the new lines for the service being onboarded (see "Composite shape" per section above).
3. Re-encrypt: `gpg --encrypt -r <operator-key-id> /tmp/<tenant>.env > vault/<tenant>.env.gpg`.
4. `shred /tmp/<tenant>.env`.
5. Commit the new `.env.gpg` (it's encrypted; safe in git).

If the per-tenant vault scheme has been replaced (Vault / AWS Secrets Manager — see `sku-execution-spec.md:150-152` open question), substitute the equivalent put-secret operation; the credential composite shapes stay the same.

---

## Rotation cadence

| Service | Cadence | Trigger |
|---|---|---|
| SendGrid | 180 d | Calendar; or any reported abuse on the brand domain |
| Zendesk | 180 d | Calendar; or on Zendesk user-role change |
| Twitter/X | 90 d | Calendar; OR if X de-platforms then we cannot rotate; have a "rate-limited posting" fallback |
| LinkedIn access token | 60 d | LinkedIn-enforced; refresh-token kicks the new 60-day window |
| LinkedIn refresh token | 365 d | LinkedIn-enforced; manual re-auth flow at this cadence |

Mirror entries into the cloud-plane `infra/secrets-manifest.md` so the quarterly secrets audit catches drift.

---

## Test-before-deploy checklist

Before flipping any tenant from "credential acquired" to "router enabled":

- [ ] Smoke `curl` succeeds for the service (per "Smoke test" block above).
- [ ] Composite shape variables are in the tenant's `.env.gpg`.
- [ ] Resolver path tested locally: `cd ~/owera-fleet && go test ./internal/skus/... -run "Test.*<Service>" -v` (or the equivalent integration test for that SKU).
- [ ] Per-tenant cost cap reviewed against the SKU's billable units (a busted Zendesk webhook should not bankrupt the tenant — the cloud-plane `CostCap` is the safety net but the per-channel rate-limit in the router is the first line).
- [ ] Tenant has signed off in writing on which channels are enabled (LinkedIn especially — posting under their brand name).

---

## Cross-references

- `docs/sku-execution-spec.md:88-135` — SKU router contract that consumes these creds.
- `internal/secrets/secrets.go` — current secrets-injection primitive.
- `~/owera-cloud/infra/secrets-manifest.md` — cloud-plane secrets register; mirror entries here when a new external service is added.
- `~/owera-cloud/api/internal/billing/stripe_ids.go` — for the billing leg of each SKU.
