#!/bin/bash
# phase06_setup_dedicated_key.sh
#
# Phase 6 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Installs the gateway's dedicated ed25519 PUBLIC key into
# /Users/hermes/.ssh/authorized_keys, ensuring future
# `ssh hermes@<host>` from the gateway uses that key (and not whatever
# initial-admin key bootstrapped the run).
#
# The gateway is responsible for generating the keypair (the keygen
# command needs an interactive TTY for the passphrase prompt) and for
# scping the .pub onto the worker before invoking this phase.
#
# Idempotent: re-running on a host that already lists the key reports
# no-change. Multiple deployments can each leave their own key.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
PUBKEY_FILE=""
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PHASE="phase06_setup_dedicated_key"

usage() {
  cat <<EOF
Usage: phase06_setup_dedicated_key.sh --pubkey PATH [options]

Worker-side fragment that installs a gateway-generated ed25519 public key
into /Users/hermes/.ssh/authorized_keys, with the directory and file in
the correct mode (700 / 600) and owned by 'hermes'.

Required:
  --pubkey PATH    Worker-side path (typically /tmp/dedicated.pub) to the
                   ed25519 public key file produced on the gateway.

Options:
  --node NAME      Node label for JSONL events. Default: \$(hostname -s)
  --hermes-user U  Override the unprivileged user. Default: hermes
  --hermes-home P  Override the user home. Default: /Users/hermes
  --dry-run        Probe only; no mutations.
  -h, --help       Show this help.

Exit codes:
  0  Key installed (or already present).
  2  Pubkey file missing or malformed.
  3  Install failed.
  4  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --pubkey)
      [ $# -ge 2 ] || { printf 'phase06: --pubkey requires a value\n' >&2; exit 4; }
      PUBKEY_FILE="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase06: --node requires a value\n' >&2; exit 4; }
      NODE_NAME="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase06: --hermes-user requires a value\n' >&2; exit 4; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase06: --hermes-home requires a value\n' >&2; exit 4; }
      HERMES_HOME="$2"; shift 2 ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase06: unknown option: %s\n' "$1" >&2
      usage >&2; exit 4 ;;
  esac
done

if [ -z "$PUBKEY_FILE" ]; then
  printf 'phase06: --pubkey is required\n' >&2
  usage >&2
  exit 4
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

info() { printf '[phase06] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

probe_pubkey() {
  local start end dur first
  start=$(now_ms)
  if [ ! -f "$PUBKEY_FILE" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "pubkey_present" "error" "$dur" "missing: $PUBKEY_FILE"
    return 1
  fi
  first=$(head -c 12 "$PUBKEY_FILE" 2>/dev/null || true)
  case "$first" in
    ssh-ed25519*|ssh-rsa*|ecdsa-sha2-*)
      end=$(now_ms); dur=$((end - start))
      log_action "pubkey_present" "no-change" "$dur" "$PUBKEY_FILE (${first}...)"
      return 0
      ;;
    *)
      end=$(now_ms); dur=$((end - start))
      log_action "pubkey_present" "error" "$dur" \
        "$PUBKEY_FILE does not look like an SSH public key (first 12 bytes: '$first')"
      return 1
      ;;
  esac
}

ensure_ssh_dir() {
  local start end dur d cur_perm cur_owner
  d="${HERMES_HOME}/.ssh"
  start=$(now_ms)
  if sudo test -d "$d" 2>/dev/null; then
    cur_perm=$(sudo stat -f '%Lp' "$d" 2>/dev/null || echo "")
    cur_owner=$(sudo stat -f '%Su' "$d" 2>/dev/null || echo "")
    if [ "$cur_perm" = "700" ] && [ "$cur_owner" = "${HERMES_USER}" ]; then
      end=$(now_ms); dur=$((end - start))
      log_action "ensure_ssh_dir" "no-change" "$dur" "$d already 700 ${HERMES_USER}"
      return 0
    fi
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_ssh_dir" "skipped" "$dur" "dry-run: would create/chmod $d"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" mkdir -p "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_ssh_dir" "error" "$dur" "mkdir -p $d failed"
    return 1
  fi
  sudo chown "${HERMES_USER}:staff" "$d" 2>/dev/null || true
  sudo chmod 700 "$d" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "ensure_ssh_dir" "ok" "$dur" "$d set to 700 ${HERMES_USER}"
  return 0
}

ensure_authorized_keys_file() {
  local start end dur f
  f="${HERMES_HOME}/.ssh/authorized_keys"
  start=$(now_ms)
  if sudo test -f "$f" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_authorized_keys_file" "no-change" "$dur" "$f exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_authorized_keys_file" "skipped" "$dur" "dry-run: would touch $f"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" touch "$f" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_authorized_keys_file" "error" "$dur" "touch $f failed"
    return 1
  fi
  sudo chmod 600 "$f" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "ensure_authorized_keys_file" "ok" "$dur" "$f created mode 600"
  return 0
}

install_key() {
  local start end dur f line auth_file
  auth_file="${HERMES_HOME}/.ssh/authorized_keys"
  f="$PUBKEY_FILE"
  start=$(now_ms)

  # Read the key (single-line form) — fail if multi-line or empty.
  line=$(head -1 "$f" 2>/dev/null)
  if [ -z "$line" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_key" "error" "$dur" "pubkey file is empty"
    return 1
  fi

  if sudo grep -qxF "$line" "$auth_file" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_key" "no-change" "$dur" "key already in $auth_file"
    return 0
  fi

  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_key" "skipped" "$dur" "dry-run: would append key to $auth_file"
    return 0
  fi

  if ! printf '%s\n' "$line" | sudo tee -a "$auth_file" >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_key" "error" "$dur" "append to $auth_file failed"
    return 1
  fi
  sudo chown "${HERMES_USER}:staff" "$auth_file" 2>/dev/null || true
  sudo chmod 600 "$auth_file" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "install_key" "ok" "$dur" "appended dedicated pubkey to $auth_file"
  return 0
}

cleanup_pubkey() {
  local start end dur
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "cleanup_pubkey" "skipped" "$dur" "dry-run: would rm $PUBKEY_FILE"
    return 0
  fi
  rm -f "$PUBKEY_FILE" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "cleanup_pubkey" "ok" "$dur" "removed $PUBKEY_FILE"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME pubkey=$PUBKEY_FILE dry_run=$DRY_RUN"

if ! probe_pubkey; then
  log_action "summary" "error" 0 "pubkey not usable"
  exit 2
fi

if ! ensure_ssh_dir; then
  log_action "summary" "error" 0 "${HERMES_HOME}/.ssh prep failed"
  exit 3
fi

if ! ensure_authorized_keys_file; then
  log_action "summary" "error" 0 "authorized_keys prep failed"
  exit 3
fi

if ! install_key; then
  log_action "summary" "error" 0 "install_key failed"
  exit 3
fi

cleanup_pubkey

log_action "summary" "ok" 0 "dedicated key installed in ${HERMES_HOME}/.ssh/authorized_keys"
info "phase06 complete"
exit 0
