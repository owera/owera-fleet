#!/bin/bash
# phase08_launchd_heartbeat.sh
#
# Phase 8 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Installs the heartbeat LaunchDaemon plist into
# /Library/LaunchDaemons/com.hermes.heartbeat.plist (system domain), and
# bootstraps it. Also tears down any leftover per-user LaunchAgent from
# an older deployment path.
#
# After install, this script waits up to 70 seconds for the first
# heartbeat file to appear under /Users/hermes/.hermes/heartbeat/.
#
# The gateway is responsible for rendering the plist (substituting
# HERMES_HOME and NODE_LABEL) and scping it onto the worker before
# invoking this phase.
#
# Idempotent: re-running on a host where the plist is already loaded and
# producing heartbeats reports no-change for each action.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
SKIP="false"
PLIST_SRC=""
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PLIST_LABEL="com.hermes.heartbeat"
PLIST_DEST="/Library/LaunchDaemons/${PLIST_LABEL}.plist"
WAIT_SECONDS=70
PHASE="phase08_launchd_heartbeat"

usage() {
  cat <<EOF
Usage: phase08_launchd_heartbeat.sh --plist PATH [options]

Worker-side fragment that installs the heartbeat LaunchDaemon (system
domain) and waits for the first heartbeat.

Required:
  --plist PATH      Worker-side path (typically /tmp/com.hermes.heartbeat.plist)
                    to the rendered plist (HERMES_HOME + NODE_LABEL
                    already substituted by the gateway).

Options:
  --node NAME       Node label for JSONL events. Default: \$(hostname -s)
  --hermes-user U   Override the unprivileged user. Default: hermes
  --hermes-home P   Override the user home. Default: /Users/hermes
  --wait-seconds N  How long to wait for the first heartbeat. Default: 70
  --skip            Skip the phase entirely (records 'skipped').
  --dry-run         Probe only; no mutations.
  -h, --help        Show this help.

Exit codes:
  0  Plist installed and first heartbeat observed (or phase skipped).
  2  Plist source missing.
  3  launchctl bootstrap failed.
  4  First heartbeat did not appear within --wait-seconds.
  5  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --plist)
      [ $# -ge 2 ] || { printf 'phase08: --plist requires a value\n' >&2; exit 5; }
      PLIST_SRC="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase08: --node requires a value\n' >&2; exit 5; }
      NODE_NAME="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase08: --hermes-user requires a value\n' >&2; exit 5; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase08: --hermes-home requires a value\n' >&2; exit 5; }
      HERMES_HOME="$2"; shift 2 ;;
    --wait-seconds)
      [ $# -ge 2 ] || { printf 'phase08: --wait-seconds requires a value\n' >&2; exit 5; }
      WAIT_SECONDS="$2"; shift 2 ;;
    --skip)
      SKIP="true"; shift ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase08: unknown option: %s\n' "$1" >&2
      usage >&2; exit 5 ;;
  esac
done

if [ -z "$NODE_NAME" ]; then
  NODE_NAME=$(hostname -s 2>/dev/null || echo "unknown")
fi

if [ "$SKIP" != "true" ] && [ -z "$PLIST_SRC" ]; then
  printf 'phase08: --plist is required (unless --skip)\n' >&2
  usage >&2
  exit 5
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

info() { printf '[phase08] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

probe_plist_src() {
  local start end dur
  start=$(now_ms)
  if [ ! -f "$PLIST_SRC" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "plist_src_present" "error" "$dur" "missing: $PLIST_SRC"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "plist_src_present" "no-change" "$dur" "$PLIST_SRC"
  return 0
}

ensure_heartbeat_dir() {
  local start end dur d
  d="${HERMES_HOME}/.hermes/heartbeat"
  start=$(now_ms)
  if sudo test -d "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_heartbeat_dir" "no-change" "$dur" "$d exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_heartbeat_dir" "skipped" "$dur" "dry-run: would mkdir $d"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" mkdir -p "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_heartbeat_dir" "error" "$dur" "mkdir -p $d failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "ensure_heartbeat_dir" "ok" "$dur" "created $d"
  return 0
}

remove_legacy_agent() {
  local start end dur legacy
  legacy="${HERMES_HOME}/Library/LaunchAgents/${PLIST_LABEL}.plist"
  start=$(now_ms)
  if ! sudo test -f "$legacy" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "remove_legacy_agent" "no-change" "$dur" "no $legacy"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "remove_legacy_agent" "skipped" "$dur" "dry-run: would rm $legacy"
    return 0
  fi
  sudo rm -f "$legacy" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "remove_legacy_agent" "ok" "$dur" "removed legacy LaunchAgent $legacy"
  return 0
}

install_plist() {
  local start end dur
  start=$(now_ms)

  # If destination already exists and matches the source, no-change.
  if sudo test -f "$PLIST_DEST" 2>/dev/null && [ -f "$PLIST_SRC" ]; then
    if sudo cmp -s "$PLIST_SRC" "$PLIST_DEST" 2>/dev/null; then
      end=$(now_ms); dur=$((end - start))
      log_action "install_plist" "no-change" "$dur" "$PLIST_DEST already up to date"
      return 0
    fi
  fi

  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_plist" "skipped" "$dur" "dry-run: would install $PLIST_SRC -> $PLIST_DEST"
    return 0
  fi

  if ! sudo mv "$PLIST_SRC" "$PLIST_DEST" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_plist" "error" "$dur" "mv $PLIST_SRC -> $PLIST_DEST failed"
    return 1
  fi
  sudo chown root:wheel "$PLIST_DEST" 2>/dev/null || true
  sudo chmod 644 "$PLIST_DEST" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "install_plist" "ok" "$dur" "installed $PLIST_DEST"
  return 0
}

reload_launchd() {
  local start end dur rc tmp_err tail
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "reload_launchd" "skipped" "$dur" \
      "dry-run: would launchctl bootout/bootstrap system $PLIST_DEST"
    return 0
  fi
  # bootout is tolerated to fail (job may not yet be loaded).
  sudo launchctl bootout system "$PLIST_DEST" >/dev/null 2>&1 || true
  tmp_err=$(mktemp -t phase08-launch.XXXXXX) || {
    log_action "reload_launchd" "error" 0 "mktemp failed"
    return 1
  }
  sudo launchctl bootstrap system "$PLIST_DEST" 2> "$tmp_err"
  rc=$?
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"
  if [ "$rc" -ne 0 ]; then
    log_action "reload_launchd" "error" "$dur" "bootstrap rc=$rc; $tail"
    return 1
  fi
  log_action "reload_launchd" "ok" "$dur" "bootstrapped system/${PLIST_LABEL}"
  return 0
}

wait_first_heartbeat() {
  local start end dur i waited hb hb_dir
  hb_dir="${HERMES_HOME}/.hermes/heartbeat"
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "first_heartbeat" "skipped" "$dur" "dry-run: skipped"
    return 0
  fi
  info "Waiting up to ${WAIT_SECONDS}s for first heartbeat at $hb_dir..."
  i=0
  waited=0
  # 5s steps; ceil(WAIT_SECONDS/5) iterations.
  while [ "$waited" -lt "$WAIT_SECONDS" ]; do
    sleep 5
    waited=$((waited + 5))
    hb=$(sudo -u "$HERMES_USER" find "$hb_dir" -type f -mmin -2 2>/dev/null | head -1)
    if [ -n "$hb" ]; then
      end=$(now_ms); dur=$((end - start))
      log_action "first_heartbeat" "ok" "$dur" "$hb (after ${waited}s)"
      return 0
    fi
    i=$((i + 1))
  done
  end=$(now_ms); dur=$((end - start))
  log_action "first_heartbeat" "error" "$dur" "no heartbeat in $hb_dir after ${WAIT_SECONDS}s"
  return 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME plist=$PLIST_SRC dest=$PLIST_DEST skip=$SKIP dry_run=$DRY_RUN"

if [ "$SKIP" = "true" ]; then
  log_action "skipped_by_flag" "skipped" 0 "--skip honoured"
  log_action "summary" "skipped" 0 "phase skipped"
  info "phase08 skipped"
  exit 0
fi

if ! probe_plist_src; then
  log_action "summary" "error" 0 "plist source missing"
  exit 2
fi

if ! ensure_heartbeat_dir; then
  log_action "summary" "error" 0 "heartbeat dir prep failed"
  exit 3
fi

remove_legacy_agent

if ! install_plist; then
  log_action "summary" "error" 0 "plist install failed"
  exit 3
fi

if ! reload_launchd; then
  log_action "summary" "error" 0 "launchd bootstrap failed"
  exit 3
fi

if ! wait_first_heartbeat; then
  log_action "summary" "error" 0 "no first heartbeat within ${WAIT_SECONDS}s"
  exit 4
fi

log_action "summary" "ok" 0 "heartbeat LaunchDaemon installed and ticking"
info "phase08 complete"
exit 0
