# Fleet health snapshot

- Generated: 2026-05-15T21:55:00Z
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
UPTIME:   18:55  up 27 days, 16:10, 3 users, load averages: 2.00 2.25 1.92
LOAD:      2.00 2.25 1.92
DISK_ROOT:
/dev/disk3s1s1   228Gi    12Gi   163Gi     7%    458k  1.7G    0%   /
MEM_AVAIL_PCT:
  4% available (22000 pages free of 488442 total)
HERMES_VERSION:
  Hermes Agent v0.13.0 (2026.5.7)
TIRITH:
  tirith 0.3.1 (at /Users/claw3/.hermes/bin/tirith)
HEARTBEAT:
  no heartbeat file at /Users/claw3/.hermes/heartbeat/claw3 (worker? probably fine. gateway? expected — gateway doesn't self-beat.)
PINNED_VERSION_FILE:
  v0.13.0
BREW_OUTDATED:
  up to date
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
UPTIME:   18:55  up 27 days, 18:03, 1 user, load averages: 1.03 1.10 1.10
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
  47s ago (claw1.local)
PINNED_VERSION_FILE:
  (not set)
BREW_OUTDATED:
  brew not installed
DISK_AT_RISK:
  (none — all real block devices < 85%)
```

## worker: hermes@claw2.local

_probe took 2000 ms; exit=0_

```
OS:       macOS 26.5 (build 25F71)
ARCH:     x86_64
KERNEL:   Darwin 25.5.0
HOSTNAME: claw2.local
UPTIME:   18:55  up  3:14, 1 user, load averages: 0.58 1.27 1.86
LOAD:      0.58 1.27 1.86
DISK_ROOT:
/dev/disk1s4s1   932Gi    12Gi   904Gi     2%    459k  4.3G    0%   /
MEM_AVAIL_PCT:
  11% available (475591 pages free of 4193607 total)
HERMES_VERSION:
  Hermes Agent v0.13.0 (2026.5.7)
TIRITH:
  tirith 0.3.1 (at /Users/hermes/.hermes/bin/tirith)
HEARTBEAT:
  59s ago (claw2.local)
PINNED_VERSION_FILE:
  (not set)
BREW_OUTDATED:
  brew not installed
DISK_AT_RISK:
  (none — all real block devices < 85%)
```
