# `claw-staging.local` — staging Mac spec + provisioning runbook

The staging Mac unblocks the Wave-9 critical-path chain:

- **C1**: live VM acceptance of `fleetctl bootstrap-worker` (PR #19 — code landed; live run gated on this Mac)
- **C2**: full end-to-end gauntlet (auth-config, swarm-e2e, markers, cronjob-alert-e2e) on a from-scratch worker
- **C3**: hermes-setup → owera-fleet cutover (the bash prototype phases out once staging proves the Go runtime parity)

Until C1 is green, the operator-plane phase-1 work is correct-by-tests but unverified against a real virgin Mac. This document is the spec + the runbook to make C1 happen.

**Owner:** Rodrigo (operator).
**Estimated time:** ~3 hours including hardware unbox + acceptance.
**Estimated cost:** ~$700 USD one-time (M3 Mac mini base).

---

## 1. Hardware

| Spec | Recommendation | Why |
|---|---|---|
| Model | **M3 Mac mini (base)** | Lowest-cost current Apple Silicon; matches the production worker (`claw1`) arch. Avoid M4 unless price-parity — Apple Silicon parity matters less than predictable behavior. |
| RAM | 16 GB | 8 GB is too tight once `claude` + `hermes` + the bootstrap toolchain share memory. 16 GB is the sweet spot. |
| Storage | 512 GB SSD | Bootstrap + Hermes runtime + restic cache + logs + headroom. 256 GB works but is uncomfortably tight by month 3. |
| Power | Always-on (no Sleep) | Behaves like prod: workers are heartbeating every 60 s; sleep makes the watchdog fire false alarms. |
| Network | Wired ethernet | Wi-Fi works but mDNS reliability is materially worse; the heartbeats-bridge depends on `claw-staging.local` being reachable. |

Order via Apple Store (1-week ship from Macapá) or local reseller if faster turnaround matters.

---

## 2. Software baseline

Match the production worker so staging is a true mirror.

| Layer | Pin to |
|---|---|
| macOS | **Same minor version as `claw1`**. Capture before flash: `ssh hermes@claw1.local sw_vers` → fill in here when ready. Stay on the same minor release for at least 3 months after Apple ships a major update. |
| Xcode CLT | `xcode-select --install` (only needed if anything compiles natively; Hermes is pre-built) |
| Homebrew | install via standard `bash -c "$(curl ...)"` flow |
| `hermes` | matches `cat ~/.hermes/PINNED_VERSION` on the gateway (current: `v0.13.0`) |
| `hermesjobs` (fleet binary) | latest `main` build from `owera-fleet` |

The `fleetctl bootstrap-worker` 10-phase pipeline handles every step from phase-01 to phase-09; do not manually install hermes or skills — let bootstrap do it. The whole point of C1 is verifying that pipeline against a virgin Mac.

---

## 3. Network setup

| Item | Value |
|---|---|
| Hostname | `claw-staging.local` |
| LAN IP | Static, in same VLAN as `claw3.local` (gateway) |
| DHCP reservation | Set on the router (UniFi / pfSense / asus / whatever) by MAC address; never rely on always-DHCP for stable mDNS |
| SSH port | Default 22 |
| mDNS | Default-on via macOS; nothing to configure beyond the hostname |
| Firewall | Default macOS firewall on; allow incoming SSH (System Settings → Network → Firewall → Options → "Remote Login" allowed) |

Test from the gateway before proceeding to user creation:

```bash
ping -c 2 claw-staging.local
ssh -o ConnectTimeout=5 claw-staging@claw-staging.local "uname -a"
```

If `claw-staging.local` doesn't resolve, debug mDNS first (`dns-sd -B _ssh._tcp` from the gateway, look for the host).

---

## 4. User setup

Two users on staging:

| User | UID | Role |
|---|---|---|
| `claw-staging` | 502 (as-built by Setup Assistant on first boot) | Admin account. SSH key from gateway's `~/.ssh/operator_ed25519.pub` in `authorized_keys`. **Username differs from the gateway** (gateway operator is `claw3`); the staging Mac uses `claw-staging` so the hostname and admin login match. |
| `hermes` | **503** | Service account that Hermes runs as. **Unprivileged** (group `staff`, not `admin`). SSH key from gateway's `~/.hermes_ssh_key.pub` in `authorized_keys`. |

> **UID 503 footnote.** Production workers (`claw1`/`claw2`) run hermes at UID 502. On staging, the admin account `claw-staging` was created at UID 502 by macOS Setup Assistant (a quirk of this Mac's first-boot setup; the more common pattern is UID 501 for the first user). Re-UID'ing a live admin account is invasive enough that we accepted hermes=503 here instead. The only practical consequence is mild noise in cross-host backup ownership comparison (numeric UIDs differ between `worker-backups/claw1/` at 502 and `worker-backups/claw-staging/` at 503); no script enforces UID 502 and `phase02_create_user.sh` supports `--uid` for exactly this case. Plumbed through `bootstrap-worker` as `--uid` in the same PR that landed this footnote.

First, seed the operator key from the gateway (one password prompt):

```bash
# On the gateway:
ssh-copy-id -i ~/.ssh/operator_ed25519.pub claw-staging@claw-staging.local
```

Then create `hermes`. Easiest path: run the canonical `§4` paste-script bundled with the operations workspace (it's idempotent, validates non-admin membership, and primes `sudo -v` so you only enter your password once):

```bash
# On the gateway — copy the script onto staging:
scp /Users/claw3/.claude/jobs/<job>/staging-step4-paste.sh \
    claw-staging@claw-staging.local:/tmp/

# Run it (one password prompt, then ~5-10 min for Homebrew on a fresh Mac):
ssh -t claw-staging@claw-staging.local 'bash /tmp/staging-step4-paste.sh'
```

The canonical inline equivalent, for the record:

```bash
# SSH in as the admin:
ssh claw-staging@claw-staging.local
sudo -v                                # one password prompt; refreshes sudo cache

# Create hermes at UID 503 (unprivileged, group staff):
RANDOM_PW="$(openssl rand -base64 24)"
sudo sysadminctl -addUser hermes -UID 503 -fullName "Hermes service" -password "$RANDOM_PW"
unset RANDOM_PW
sudo dseditgroup -o edit -a hermes -t user staff
# Verify hermes is NOT in admin (must print "no"):
dseditgroup -o checkmember -m hermes admin

# Install Homebrew under hermes (phase 00 expects brew on PATH):
sudo -u hermes -i bash -c '/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"'

# Authorize the hermes runtime key (from the gateway's ~/.hermes_ssh_key.pub):
sudo mkdir -p /Users/hermes/.ssh
sudo tee /Users/hermes/.ssh/authorized_keys >/dev/null <<'PUBKEY'
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOmg3QaNw8AOu4q+hhMqoXi3BePUVBh1hSxmMthutyLC hermes-gateway-claw3
PUBKEY
sudo chmod 700 /Users/hermes/.ssh
sudo chmod 600 /Users/hermes/.ssh/authorized_keys
sudo chown -R hermes:staff /Users/hermes/.ssh
```

Verify from the gateway:

```bash
ssh hermes@claw-staging.local "id"
# expect: uid=503(hermes) gid=20(staff) ...
```

> **Why no `-admin`:** the hermes service account must be unprivileged. `bootstrap-worker`'s phase02 enforces this; pass `--allow-existing-admin-user` only if you have a deliberate reason (and you almost never do).

---

## 5. Bootstrap — the actual test

This is the C1 acceptance criterion. Run from the gateway:

```bash
cd ~/owera-fleet
go run ./cmd/fleetctl bootstrap-worker --node hermes@claw-staging.local --uid 503 --verbose \
  2>&1 | tee /tmp/bootstrap-staging.log
```

The 10 phases (00 brew → 09 verify) should complete in 8-15 minutes. Each phase writes a structured JSONL line. The pipeline is idempotent — re-running after a failed phase resumes from the same point.

> **`--uid 503` rationale:** see §4 UID 503 footnote. Production workers use the runner's default (502); staging needs the override because UID 502 was occupied by the admin account at first boot.

Per-phase expected behavior (matches the wave-9 Go port):

| Phase | What's expected |
|---|---|
| 00 brew_baseline | Homebrew + essentials (`jq coreutils shellcheck gh wget ripgrep`) baseline; phase 00 also installs brew if §4 didn't already |
| 01 verify_prereqs | `sudo` / `brew` / `xcode-clt` reachable for `hermes` |
| 02 create_user | `hermes` at UID 503, /bin/zsh, /Users/hermes home, group=staff (NOT admin); reports `no-change` since §4 already created it |
| 03 install_hermes | Downloads `hermes` matching `PINNED_VERSION` (v0.13.0); seeds `~/.hermes/PINNED_VERSION` |
| 04 seed_config | Drops `config.yaml` bundle; rewrites `tirith_path` from gateway prefix to `/Users/hermes/.hermes` |
| 05 distribute_secrets | Installs `.env.gpg` (mode 600) + `run-with-secrets.sh` (mode 700) |
| 06 setup_dedicated_key | Installs gateway pubkey in `/Users/hermes/.ssh/authorized_keys` (idempotent — §4 already did this) |
| 07 install_tirith | Best-effort `brew install tirith`, falls back to Hermes-skill path |
| 08 launchd_heartbeat | Installs `com.hermes.heartbeat` LaunchDaemon (every 60 s touch); waits up to 70 s for first tick |
| 09 verify | Final acceptance gate: `hermes version` pinned, non-root user, fresh heartbeat. Emits final `verify` JSONL event |

A clean run looks like:

```
[bootstrap] hermes@claw-staging.local phase=00 result=ok duration_ms=4521
[bootstrap] hermes@claw-staging.local phase=01 result=ok duration_ms=89
...
[bootstrap] hermes@claw-staging.local phase=09 result=ok duration_ms=2147
[bootstrap] hermes@claw-staging.local COMPLETE total=541s
```

---

## 6. Acceptance — health snapshot parity

After bootstrap, the staging Mac must look identical to `claw1` / `claw2` in the gateway's view.

```bash
# From the gateway:
~/hermes-setup/scripts/fleet-health-snapshot.sh --quiet
ls -lh ~/.hermes/reports/ | tail -1
# open the new fleet-snapshot-<timestamp>.md and verify claw-staging appears
```

Required parity items (the snapshot will surface diffs):

- [ ] `hermes` version = `v0.13.0` (matches `claw1`/`claw2`)
- [ ] Heartbeat freshness < 90 s (≤ 1 missed beat)
- [ ] `top` doesn't show abnormal CPU/MEM
- [ ] Disk free > 50 GB
- [ ] `brew doctor` clean (no warnings)
- [ ] `~/.hermes/PINNED_VERSION` content matches gateway

Add `hermes@claw-staging.local` to `~/.hermes/nodes.txt`:

```bash
echo "hermes@claw-staging.local" >> ~/.hermes/nodes.txt
```

Now the watchdog, snapshot publisher, and worker-state backup will all include staging on their next tick.

---

## 7. C2 gauntlet (the productized test pass)

With staging up, run the full Phase-1/Phase-2 acceptance suite against it (these are the tests that previously ran in `go test` mocks; now they run on a real Mac):

```bash
cd ~/owera-fleet
go run ./cmd/fleetctl test --tier=e2e --node=claw-staging.local 2>&1 | tee /tmp/c2-gauntlet.log
```

Scenarios that must pass on staging:

- usecase31 — orchestrator/swarm round-trip via SSH-attached worker
- usecase32 — auth-config audit reproduces the SECURITY_NOTES drift table
- usecase33 — markers CLI content-hash round-trip
- usecase34 — cronjob → alert end-to-end

A failure on any of these against a fresh Mac is more interesting than a failure in CI — it means our Phase-1 work has a hidden host-environment dependency that needs to be either captured in bootstrap or documented as a non-portable assumption.

---

## 8. Cutover (C3) — hermes-setup → owera-fleet

Only after C1 + C2 both green. Out of scope for this doc; tracked in `owera-fleet/knowing-all-you-now-calm-leaf.md` → "hermes-setup phase-out".

The staging Mac becomes the second cutover target (after `claw1` and `claw2`) and is where the cutover playbook is dry-run before touching prod workers.

---

## 9. Operational notes (post-acceptance)

Once staging is healthy:

- **Production smoke target.** Every release-candidate of `hermesjobs` / `fleetctl` lands on staging first via `update-hermes-fleet.sh --target staging-only` (need to add this flag; today it's all-or-nothing).
- **Layer-2 probe target.** `fleet-readiness-probes.sh` and Layer-2 GAP regressions (P1/P7/P8) can be tested against staging without risk to prod workloads.
- **Disposable.** Staging is the only host that can be cleanly `rm -rf ~/.hermes && bootstrap-worker` for re-running bootstrap from scratch. Use it for any "what if bootstrap had been ..." experiments.

---

## Cross-references

- `~/owera-fleet/cmd/fleetctl/bootstrap_worker.go` — 10-phase pipeline driver
- `~/owera-fleet/docs/roadmap.md` — Wave-9 PR #19 (bootstrap phases) and C1/C2/C3 phase notes
- `~/hermes-setup/scripts/lib/phases.sh` — original bash phases that the Go port mirrors
- `~/hermes-setup/SECURITY_NOTES.md` — drift table that `audit config` reproduces; staging's audit-config run is part of C1 acceptance
