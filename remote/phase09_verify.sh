#!/bin/bash
# phase09_verify.sh
#
# Phase 9 of the bootstrap pipeline — the acceptance gate. Runs ON A
# WORKER after being scp'd by the gateway. Asserts the post-bootstrap
# invariants:
#
#   1. `hermes version` resolves under the unprivileged user AND matches
#      the pinned version.
#   2. `tirith --version` resolves (or is acknowledged as pending — Hermes
#      auto-fetches on first use per SECURITY_NOTES).
#   3. The unprivileged user is non-root and not in the admin group.
#   4. A heartbeat file < 2 minutes old exists in the heartbeat dir.
#
# Idempotent: this phase is read-only. Re-running emits identical events.
#
# Emits a final `verify` JSONL event with the rolled-up result.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
PINNED_VERSION=""
SKIP_TIRITH="false"
SKIP_HEARTBEAT="false"
ALLOW_ADMIN="false"
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PHASE="phase09_verify"

usage() {
  cat <<EOF
Usage: phase09_verify.sh --pinned-version TAG [options]

Worker-side acceptance gate. Asserts hermes/tirith/user/heartbeat
invariants are all true post-bootstrap.

Required:
  --pinned-version TAG    Pinned Hermes version to match against.

Options:
  --node NAME             Node label for JSONL events. Default: \$(hostname -s)
  --hermes-user U         Override the unprivileged user. Default: hermes
  --hermes-home P         Override the user home. Default: /Users/hermes
  --skip-tirith           Don't check tirith.
  --skip-heartbeat        Don't check heartbeat freshness.
  --allow-existing-admin-user  Don't fail if user is in admin group.
  --dry-run               Probe-only (this phase is already read-only;
                          flag retained for orchestrator-uniformity).
  -h, --help              Show this help.

Exit codes:
  0  All checks passed.
  2  hermes version probe failed or mismatched.
  3  user/group invariant failed.
  4  heartbeat stale or missing.
  5  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --pinned-version)
      [ $# -ge 2 ] || { printf 'phase09: --pinned-version requires a value\n' >&2; exit 5; }
      PINNED_VERSION="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase09: --node requires a value\n' >&2; exit 5; }
      NODE_NAME="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase09: --hermes-user requires a value\n' >&2; exit 5; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase09: --hermes-home requires a value\n' >&2; exit 5; }
      HERMES_HOME="$2"; shift 2 ;;
    --skip-tirith)
      SKIP_TIRITH="true"; shift ;;
    --skip-heartbeat)
      SKIP_HEARTBEAT="true"; shift ;;
    --allow-existing-admin-user)
      ALLOW_ADMIN="true"; shift ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase09: unknown option: %s\n' "$1" >&2
      usage >&2; exit 5 ;;
  esac
done

if [ -z "$PINNED_VERSION" ]; then
  printf 'phase09: --pinned-version is required\n' >&2
  usage >&2
  exit 5
fi

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

info() { printf '[phase09] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Probes
# ---------------------------------------------------------------------------

probe_hermes_version() {
  local start end dur v
  start=$(now_ms)
  v=$(sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env"; hermes version 2>/dev/null | head -1' \
    2>/dev/null || true)
  end=$(now_ms); dur=$((end - start))
  if [ -z "$v" ]; then
    log_action "hermes_version" "error" "$dur" "no output from hermes version as ${HERMES_USER}"
    return 1
  fi
  if ! printf '%s' "$v" | grep -qF "$PINNED_VERSION"; then
    log_action "hermes_version_pinned" "error" "$dur" "got=$v want=$PINNED_VERSION"
    return 1
  fi
  log_action "hermes_version_pinned" "no-change" "$dur" "$v"
  return 0
}

probe_tirith() {
  local start end dur t
  start=$(now_ms)
  t=$(sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env"; tirith --version 2>/dev/null || echo MISSING' \
    2>/dev/null || echo "MISSING")
  end=$(now_ms); dur=$((end - start))
  case "$t" in
    MISSING|"")
      # Hermes auto-fetches tirith at first use; not a fail. Mirrors prototype.
      log_action "tirith_present" "warn" "$dur" "not yet installed; auto-fetched by Hermes at runtime"
      return 0
      ;;
    *)
      log_action "tirith_present" "no-change" "$dur" "$t"
      return 0
      ;;
  esac
}

probe_user_role() {
  local start end dur uid groups
  start=$(now_ms)
  uid=$(sudo -u "$HERMES_USER" -i sh -c 'id -u' 2>/dev/null || echo "")
  groups=$(sudo -u "$HERMES_USER" -i sh -c 'id -Gn 2>/dev/null' 2>/dev/null || echo "")
  end=$(now_ms); dur=$((end - start))
  if [ "$uid" = "0" ]; then
    log_action "non_root_user" "error" "$dur" "uid=0 — should be a non-root user"
    return 1
  fi
  log_action "non_root_user" "no-change" "$dur" "uid=$uid"

  start=$(now_ms)
  if printf '%s' "$groups" | tr ' ' '\n' | grep -qx admin; then
    end=$(now_ms); dur=$((end - start))
    if [ "$ALLOW_ADMIN" = "true" ]; then
      log_action "non_admin_user" "warn" "$dur" "${HERMES_USER} in admin group (allowed by --allow-existing-admin-user)"
      return 0
    fi
    log_action "non_admin_user" "error" "$dur" "${HERMES_USER} is in admin group: $groups"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "non_admin_user" "no-change" "$dur" "groups: $groups"
  return 0
}

probe_heartbeat_fresh() {
  local start end dur hb hb_dir
  hb_dir="${HERMES_HOME}/.hermes/heartbeat"
  start=$(now_ms)
  hb=$(sudo -u "$HERMES_USER" find "$hb_dir" -type f -mmin -2 2>/dev/null | head -1)
  end=$(now_ms); dur=$((end - start))
  if [ -z "$hb" ]; then
    log_action "heartbeat_fresh" "error" "$dur" "no heartbeat file < 2 min old in $hb_dir"
    return 1
  fi
  log_action "heartbeat_fresh" "no-change" "$dur" "$hb"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME pinned=$PINNED_VERSION user=$HERMES_USER dry_run=$DRY_RUN"

OVERALL=0

if ! probe_hermes_version; then
  OVERALL=2
fi

if [ "$SKIP_TIRITH" != "true" ]; then
  probe_tirith || true
fi

if ! probe_user_role; then
  [ "$OVERALL" -eq 0 ] && OVERALL=3
fi

if [ "$SKIP_HEARTBEAT" != "true" ]; then
  if ! probe_heartbeat_fresh; then
    [ "$OVERALL" -eq 0 ] && OVERALL=4
  fi
fi

if [ "$OVERALL" -eq 0 ]; then
  # Emit the canonical 'verify' summary event the prototype's
  # phase_verify ends on.
  log_action "verify" "ok" 0 "node=${NODE_NAME} pinned=${PINNED_VERSION}"
  log_action "summary" "ok" 0 "all acceptance probes passed"
  info "phase09 complete (PASS)"
  exit 0
fi

log_action "verify" "error" 0 "exit_code=$OVERALL"
log_action "summary" "error" 0 "acceptance gate failed (exit=$OVERALL)"
info "phase09 complete (FAIL exit=$OVERALL)"
exit "$OVERALL"
