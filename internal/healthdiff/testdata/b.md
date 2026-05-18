# Fleet health snapshot

- Generated: 2026-05-15T22:00:09Z
- Gateway:   claw3.local (claw3)
- Workers:   2 (hermes@claw1.local hermes@claw2.local)

---

## gateway: claw3

_probe took 2000 ms; exit=0_

```
OS:       macOS 26.4.1 (build 25E253)
ARCH:     arm64
KERNEL:   Darwin 25.4.0
HOSTNAME: claw3.local
UPTIME:   19:00  up 27 days, 16:15, 3 users, load averages: 2.00 2.25 1.92
LOAD:      2.00 2.25 1.92
DISK_ROOT:
/dev/disk3s1s1   228Gi    14Gi   161Gi     8%    458k  1.7G    0%   /
MEM_AVAIL_PCT:
  2% available (11187 pages free of 488442 total)
HERMES_VERSION:
  Hermes Agent v0.13.0 (2026.5.7)
TIRITH:
  tirith 0.3.1 (at /Users/claw3/.hermes/bin/tirith)
HEARTBEAT:
  no heartbeat file at /Users/claw3/.hermes/heartbeat/claw3 (worker? probably fine. gateway? expected — gateway doesn't self-beat.)
PINNED_VERSION_FILE:
  v0.13.0
BREW_OUTDATED:
  3 outdated:
    gh
    go
    ripgrep
DISK_AT_RISK:
  (none — all real block devices < 85%)
```

## worker: hermes@claw1.local

_probe took 1000 ms; exit=0_

```
OS:       macOS 26.4.1 (build 25E253)
ARCH:     arm64
KERNEL:   Darwin 25.4.0
HOSTNAME: claw1.local
UPTIME:   19:00  up 27 days, 18:08, 1 user, load averages: 1.03 1.10 1.10
LOAD:      1.03 1.10 1.10
DISK_ROOT:
/dev/disk3s1s1   926Gi    12Gi   881Gi     2%    458k  4.3G    0%   /
MEM_AVAIL_PCT:
  9% available (96586 pages free of 999847 total)
HERMES_VERSION:
  Hermes Agent v0.13.0 (2026.5.7)
TIRITH:
  tirith 0.3.1 (at /Users/hermes/.hermes/bin/tirith)
HEARTBEAT:
  no heartbeat file at /Users/hermes/.hermes/heartbeat/claw1
PINNED_VERSION_FILE:
  (not set)
BREW_OUTDATED:
  brew not installed
DISK_AT_RISK:
  (none — all real block devices < 85%)
```

## worker: hermes@claw3.local

_probe took 1100 ms; exit=0_

```
OS:       macOS 26.4.1 (build 25E253)
ARCH:     arm64
KERNEL:   Darwin 25.4.0
HOSTNAME: claw3.local
UPTIME:   19:00  up 1 day, 0:05, 1 user, load averages: 0.50 0.40 0.30
LOAD:      0.50 0.40 0.30
DISK_ROOT:
/dev/disk3s1s1   500Gi    10Gi   480Gi     2%    458k  4.3G    0%   /
MEM_AVAIL_PCT:
  20% available (200000 pages free of 1000000 total)
HERMES_VERSION:
  Hermes Agent v0.13.0 (2026.5.7)
TIRITH:
  tirith 0.3.1 (at /Users/hermes/.hermes/bin/tirith)
HEARTBEAT:
  10s ago (claw3.local)
PINNED_VERSION_FILE:
  (not set)
BREW_OUTDATED:
  brew not installed
DISK_AT_RISK:
  (none — all real block devices < 85%)
```
