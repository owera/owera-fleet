#!/bin/bash
# phase01_verify_prereqs.sh
#
# Phase 1 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Verifies the host is reachable, that sudo works for the
# initial admin account (passwordless or with cached credentials), and
# that Homebrew is now present (phase00 should have ensured this).
#
# Self-contained: no gateway lib paths. Mirrors phase00's idempotency +
# JSONL telemetry contract.
#
# Actions emitted:
#   ssh_reachable    — implied by the fact that this script ran. Records
#                      hostname + uname.
#   sudo_works       — `sudo -n true` to confirm cached/no-password sudo.
#                      If that fails, we emit `error` (operator should run
#                      `ssh <node> -- sudo -v` from the gateway).
#   brew_present     — repeats phase00's probe but FAILS if missing
#                      (phase00 should have already installed it).
#   xcode_clt        — `xcode-select -p` must resolve.
#   summary          — terminal roll-up.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
PHASE="phase01_verify_prereqs"

usage() {
  cat <<EOF
Usage: phase01_verify_prereqs.sh [options]

Worker-side prereq verification. Asserts sudo works, brew is present, and
Xcode CLT is installed.

Options:
  --node NAME    Override the node label emitted in JSONL events.
                 Default: \$(hostname -s)
  --dry-run      Probe only; same as a normal run since this phase never
                 mutates. Still recognised for orchestrator-uniformity.
  -h, --help     Show this help.

Exit codes:
  0  All probes passed.
  2  sudo is not configured for non-interactive use.
  3  Homebrew is missing (phase00 should have run first).
  4  Xcode Command Line Tools missing.
  5  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --node)
      [ $# -ge 2 ] || { printf 'phase01: --node requires a value\n' >&2; exit 5; }
      NODE_NAME="$2"
      shift 2
      ;;
    --dry-run)
      DRY_RUN="true"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'phase01: unknown option: %s\n' "$1" >&2
      usage >&2
      exit 5
      ;;
  esac
done

if [ -z "$NODE_NAME" ]; then
  NODE_NAME=$(hostname -s 2>/dev/null || echo "unknown")
fi

# ---------------------------------------------------------------------------
# Inline JSONL emitter (mirrors phase00).
# ---------------------------------------------------------------------------

MAX_STDERR_TAIL=4096

_iso_now() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

_json_escape() {
  local s="$1"
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=$(printf '%s' "$s" | tr '\n\r\t' '   ')
  printf '%s' "$s"
}

log_action() {
  local action result duration_ms tail tail_esc tail_len
  action="$1"
  result="$2"
  duration_ms="${3:-0}"
  tail="${4:-}"
  tail_len=${#tail}
  if [ "$tail_len" -gt "$MAX_STDERR_TAIL" ]; then
    tail=${tail:$((tail_len - MAX_STDERR_TAIL))}
  fi
  tail_esc=$(_json_escape "$tail")
  if [ -z "$tail_esc" ]; then
    printf '{"ts":"%s","node":"%s","phase":"%s","action":"%s","result":"%s","duration_ms":%s}\n' \
      "$(_iso_now)" "$NODE_NAME" "$PHASE" "$action" "$result" "$duration_ms" >&2
  else
    printf '{"ts":"%s","node":"%s","phase":"%s","action":"%s","result":"%s","duration_ms":%s,"stderr_tail":"%s"}\n' \
      "$(_iso_now)" "$NODE_NAME" "$PHASE" "$action" "$result" "$duration_ms" "$tail_esc" >&2
  fi
}

info() {
  printf '[phase01] %s\n' "$*"
}

now_ms() {
  local s
  s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Probes
# ---------------------------------------------------------------------------

probe_ssh_reachable() {
  local start end dur u
  start=$(now_ms)
  u=$(uname -srm 2>/dev/null || echo "uname-failed")
  end=$(now_ms); dur=$((end - start))
  log_action "ssh_reachable" "no-change" "$dur" "$u on $NODE_NAME"
  return 0
}

probe_sudo_works() {
  local start end dur
  start=$(now_ms)
  if sudo -n true >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "sudo_works" "no-change" "$dur" "sudo -n true succeeded"
    return 0
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "sudo_works" "error" "$dur" "sudo -n true failed; operator must run 'ssh <node> -- sudo -v' (or pre-cache password) before rerunning phase01"
  return 1
}

probe_brew_present() {
  local start end dur p ver
  start=$(now_ms)
  for p in /opt/homebrew/bin/brew /usr/local/bin/brew; do
    if [ -x "$p" ]; then
      ver=$("$p" --version 2>/dev/null | head -1)
      end=$(now_ms); dur=$((end - start))
      log_action "brew_present" "no-change" "$dur" "$ver at $p"
      return 0
    fi
  done
  end=$(now_ms); dur=$((end - start))
  log_action "brew_present" "error" "$dur" "Homebrew not found; run phase00_brew_baseline.sh (or the official installer as admin) first"
  return 1
}

probe_xcode_clt() {
  local start end dur clt_path
  start=$(now_ms)
  clt_path=$(xcode-select -p 2>/dev/null || true)
  end=$(now_ms); dur=$((end - start))
  if [ -n "$clt_path" ] && [ -d "$clt_path" ]; then
    log_action "xcode_clt" "no-change" "$dur" "CLT at $clt_path"
    return 0
  fi
  log_action "xcode_clt" "error" "$dur" "Xcode CLT missing; run 'xcode-select --install' (dialog on the node's display) before rerunning"
  return 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME dry_run=$DRY_RUN"

# Dry-run uniformity: this phase is read-only, so dry-run runs identically.
probe_ssh_reachable

if ! probe_sudo_works; then
  log_action "summary" "error" 0 "sudo precondition failed"
  exit 2
fi

if ! probe_brew_present; then
  log_action "summary" "error" 0 "brew precondition failed"
  exit 3
fi

if ! probe_xcode_clt; then
  log_action "summary" "error" 0 "xcode-clt precondition failed"
  exit 4
fi

log_action "summary" "ok" 0 "all prereqs satisfied"
info "phase01 complete"
exit 0
