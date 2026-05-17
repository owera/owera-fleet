#!/bin/bash
# phase07_install_tirith.sh
#
# Phase 7 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Best-effort install of the Tirith auth-proxy binary on the
# worker:
#   1. If `tirith --version` already resolves (under the hermes user's
#      installer env), report no-change.
#   2. Else try `brew install tirith`.
#   3. Else try `hermes skill run hermes-container-security` to let the
#      Hermes-managed install path handle it.
#   4. Else log a warn and exit 0 — Hermes will auto-fetch on first use
#      per SECURITY_NOTES.
#
# Idempotent: every step probes before mutating. The phase never fails
# the bootstrap on Tirith specifically (the prototype intentionally
# converted this from a fail-stop into a warn).
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
SKIP="false"
HERMES_USER="hermes"
PHASE="phase07_install_tirith"

usage() {
  cat <<EOF
Usage: phase07_install_tirith.sh [options]

Worker-side fragment that best-effort installs the Tirith auth-proxy.

Options:
  --node NAME      Node label for JSONL events. Default: \$(hostname -s)
  --hermes-user U  Override the unprivileged user. Default: hermes
  --skip           Skip the phase entirely (records 'skipped').
  --dry-run        Probe only; no installation attempts.
  -h, --help       Show this help.

Exit codes:
  0  Tirith installed, already installed, deferred to runtime, or skipped.
  4  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --node)
      [ $# -ge 2 ] || { printf 'phase07: --node requires a value\n' >&2; exit 4; }
      NODE_NAME="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase07: --hermes-user requires a value\n' >&2; exit 4; }
      HERMES_USER="$2"; shift 2 ;;
    --skip)
      SKIP="true"; shift ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase07: unknown option: %s\n' "$1" >&2
      usage >&2; exit 4 ;;
  esac
done

if [ -z "$NODE_NAME" ]; then
  NODE_NAME=$(hostname -s 2>/dev/null || echo "unknown")
fi

# ---------------------------------------------------------------------------
# Inline JSONL emitter
# ---------------------------------------------------------------------------

MAX_STDERR_TAIL=4096

_iso_now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

_json_escape() {
  local s="$1"
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=$(printf '%s' "$s" | tr '\n\r\t' '   ')
  printf '%s' "$s"
}

log_action() {
  local action result duration_ms tail tail_esc tail_len
  action="$1"; result="$2"; duration_ms="${3:-0}"; tail="${4:-}"
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

info() { printf '[phase07] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

current_tirith_version() {
  sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env"; tirith --version 2>/dev/null' \
    2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

probe_already_installed() {
  local start end dur v
  start=$(now_ms)
  v=$(current_tirith_version)
  end=$(now_ms); dur=$((end - start))
  if [ -n "$v" ]; then
    log_action "already_installed" "no-change" "$dur" "$v"
    return 0
  fi
  log_action "already_installed" "no-change" "$dur" "tirith not yet on PATH"
  return 1
}

try_brew_install() {
  local start end dur tmp_err rc tail
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "brew_install" "skipped" "$dur" "dry-run: would brew install tirith"
    return 1
  fi
  tmp_err=$(mktemp -t phase07-brew.XXXXXX) || {
    log_action "brew_install" "error" 0 "mktemp failed"
    return 1
  }
  sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env";
     command -v brew >/dev/null 2>&1 && brew install tirith' \
    >/dev/null 2> "$tmp_err"
  rc=$?
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"
  if [ "$rc" -eq 0 ] && [ -n "$(current_tirith_version)" ]; then
    log_action "brew_install" "ok" "$dur" "brew install tirith succeeded"
    return 0
  fi
  log_action "brew_install" "no-change" "$dur" "rc=$rc; tail=$tail"
  return 1
}

try_hermes_skill_install() {
  local start end dur tmp_err rc tail
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "hermes_skill_install" "skipped" "$dur" "dry-run: would hermes skill run hermes-container-security"
    return 1
  fi
  tmp_err=$(mktemp -t phase07-skill.XXXXXX) || {
    log_action "hermes_skill_install" "error" 0 "mktemp failed"
    return 1
  }
  sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env";
     command -v hermes >/dev/null 2>&1 && hermes skill run hermes-container-security' \
    >/dev/null 2> "$tmp_err"
  rc=$?
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"
  if [ "$rc" -eq 0 ] && [ -n "$(current_tirith_version)" ]; then
    log_action "hermes_skill_install" "ok" "$dur" "hermes skill install succeeded"
    return 0
  fi
  log_action "hermes_skill_install" "no-change" "$dur" "rc=$rc; tail=$tail"
  return 1
}

verify_installed() {
  local start end dur v
  start=$(now_ms)
  v=$(current_tirith_version)
  end=$(now_ms); dur=$((end - start))
  if [ -n "$v" ]; then
    log_action "verify_installed" "no-change" "$dur" "$v"
    return 0
  fi
  log_action "verify_installed" "warn" "$dur" \
    "tirith not installed pre-runtime; Hermes will auto-fetch on first use (per SECURITY_NOTES)"
  return 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME hermes_user=$HERMES_USER skip=$SKIP dry_run=$DRY_RUN"

if [ "$SKIP" = "true" ]; then
  log_action "skipped_by_flag" "skipped" 0 "--skip honoured"
  log_action "summary" "skipped" 0 "phase skipped"
  info "phase07 skipped"
  exit 0
fi

if probe_already_installed; then
  log_action "summary" "no-change" 0 "tirith already installed"
  info "phase07 complete (no-op)"
  exit 0
fi

# Best-effort: try brew, then hermes skill, then defer.
if try_brew_install; then
  log_action "summary" "ok" 0 "tirith installed via brew"
  info "phase07 complete"
  exit 0
fi

if try_hermes_skill_install; then
  log_action "summary" "ok" 0 "tirith installed via hermes skill"
  info "phase07 complete"
  exit 0
fi

# Final verification — emits warn if tirith still missing, but phase exits 0
# because Hermes auto-fetches on first use.
verify_installed || true
log_action "summary" "warn" 0 "tirith install deferred to Hermes runtime"
info "phase07 complete (deferred)"
exit 0
