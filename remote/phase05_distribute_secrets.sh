#!/bin/bash
# phase05_distribute_secrets.sh
#
# Phase 5 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Installs the encrypted .env.gpg bundle and the
# run-with-secrets.sh wrapper under /Users/hermes/.hermes/. Plaintext
# secrets are never written to disk — the wrapper decrypts at runtime
# only into the environment of the wrapped command.
#
# The orchestrator scp's both the gpg blob and the wrapper to /tmp/ first;
# this script installs them with the right ownership and mode, then
# removes the staging copies.
#
# Idempotent: re-running on a host that already has both artifacts in
# the right shape reports no-change.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
ENV_GPG_SRC=""
WRAPPER_SRC=""
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PHASE="phase05_distribute_secrets"

usage() {
  cat <<EOF
Usage: phase05_distribute_secrets.sh --env-gpg PATH --wrapper PATH [options]

Worker-side fragment that installs .env.gpg + run-with-secrets.sh into
/Users/hermes/.hermes/.

Required:
  --env-gpg PATH   Worker-side path (typically /tmp/.env.gpg) to the
                   encrypted env bundle.
  --wrapper PATH   Worker-side path (typically /tmp/run-with-secrets.sh)
                   to the runtime decryption wrapper.

Options:
  --node NAME      Node label for JSONL events. Default: \$(hostname -s)
  --hermes-user U  Override the unprivileged user. Default: hermes
  --hermes-home P  Override the user home. Default: /Users/hermes
  --dry-run        Probe only; no mutations.
  -h, --help       Show this help.

Exit codes:
  0  Both artifacts installed (or already correct).
  2  Source file missing on worker.
  3  Install (mv / chmod / chown) failed.
  4  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --env-gpg)
      [ $# -ge 2 ] || { printf 'phase05: --env-gpg requires a value\n' >&2; exit 4; }
      ENV_GPG_SRC="$2"; shift 2 ;;
    --wrapper)
      [ $# -ge 2 ] || { printf 'phase05: --wrapper requires a value\n' >&2; exit 4; }
      WRAPPER_SRC="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase05: --node requires a value\n' >&2; exit 4; }
      NODE_NAME="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase05: --hermes-user requires a value\n' >&2; exit 4; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase05: --hermes-home requires a value\n' >&2; exit 4; }
      HERMES_HOME="$2"; shift 2 ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase05: unknown option: %s\n' "$1" >&2
      usage >&2; exit 4 ;;
  esac
done

if [ -z "$ENV_GPG_SRC" ] || [ -z "$WRAPPER_SRC" ]; then
  printf 'phase05: both --env-gpg and --wrapper are required\n' >&2
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

info() { printf '[phase05] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

probe_sources() {
  local start end dur missing
  missing=""
  start=$(now_ms)
  [ -f "$ENV_GPG_SRC" ] || missing="${missing} $ENV_GPG_SRC"
  [ -f "$WRAPPER_SRC" ] || missing="${missing} $WRAPPER_SRC"
  end=$(now_ms); dur=$((end - start))
  if [ -n "$missing" ]; then
    log_action "sources_present" "error" "$dur" "missing:${missing}"
    return 1
  fi
  log_action "sources_present" "no-change" "$dur" "$ENV_GPG_SRC, $WRAPPER_SRC"
  return 0
}

ensure_scripts_dir() {
  local start end dur d
  d="${HERMES_HOME}/.hermes/scripts"
  start=$(now_ms)
  if sudo -u "$HERMES_USER" test -d "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_scripts_dir" "no-change" "$dur" "$d exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_scripts_dir" "skipped" "$dur" "dry-run: would mkdir $d"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" mkdir -p "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_scripts_dir" "error" "$dur" "mkdir -p $d failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "ensure_scripts_dir" "ok" "$dur" "created $d"
  return 0
}

# Move src to dest with the requested mode + owner. Idempotent: if
# dest already exists with the right mode and owner AND no source is
# pending, report no-change.
install_artifact() {
  local action_name="$1" src="$2" dest="$3" mode="$4"
  local start end dur cur_perm cur_owner want_owner expected
  want_owner="${HERMES_USER}"
  expected="${mode} ${want_owner}"
  start=$(now_ms)

  # If src no longer exists and dest already has correct shape, no-change.
  if [ ! -f "$src" ] && sudo test -f "$dest" 2>/dev/null; then
    cur_perm=$(sudo stat -f '%Lp' "$dest" 2>/dev/null || echo "")
    cur_owner=$(sudo stat -f '%Su' "$dest" 2>/dev/null || echo "")
    if [ "$cur_perm" = "$mode" ] && [ "$cur_owner" = "$want_owner" ]; then
      end=$(now_ms); dur=$((end - start))
      log_action "$action_name" "no-change" "$dur" "$dest already mode $mode owner $want_owner"
      return 0
    fi
  fi

  if [ ! -f "$src" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "$action_name" "error" "$dur" "source $src missing"
    return 1
  fi

  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "$action_name" "skipped" "$dur" "dry-run: would install $src -> $dest mode $mode owner $want_owner"
    return 0
  fi

  if ! sudo mv "$src" "$dest" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "$action_name" "error" "$dur" "mv $src -> $dest failed"
    return 1
  fi
  if ! sudo chown "${want_owner}:staff" "$dest" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "$action_name" "error" "$dur" "chown $dest failed"
    return 1
  fi
  if ! sudo chmod "$mode" "$dest" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "$action_name" "error" "$dur" "chmod $mode $dest failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "$action_name" "ok" "$dur" "installed $dest ($expected)"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME env_gpg=$ENV_GPG_SRC wrapper=$WRAPPER_SRC dry_run=$DRY_RUN"

# Probe is forgiving: if BOTH sources are missing but BOTH destinations
# are correctly installed, that's a valid no-change re-run.
ENV_DEST="${HERMES_HOME}/.hermes/.env.gpg"
WRAPPER_DEST="${HERMES_HOME}/.hermes/scripts/run-with-secrets.sh"
RERUN_NO_OP=0
if [ ! -f "$ENV_GPG_SRC" ] && [ ! -f "$WRAPPER_SRC" ] \
   && sudo test -f "$ENV_DEST" 2>/dev/null \
   && sudo test -f "$WRAPPER_DEST" 2>/dev/null; then
  RERUN_NO_OP=1
fi

if [ "$RERUN_NO_OP" -eq 0 ]; then
  if ! probe_sources; then
    log_action "summary" "error" 0 "source artifacts not staged on worker"
    exit 2
  fi
fi

if ! ensure_scripts_dir; then
  log_action "summary" "error" 0 "scripts dir create failed"
  exit 3
fi

if ! install_artifact "install_env_gpg" "$ENV_GPG_SRC" "$ENV_DEST" "600"; then
  log_action "summary" "error" 0 ".env.gpg install failed"
  exit 3
fi

if ! install_artifact "install_wrapper" "$WRAPPER_SRC" "$WRAPPER_DEST" "700"; then
  log_action "summary" "error" 0 "run-with-secrets.sh install failed"
  exit 3
fi

log_action "summary" "ok" 0 "secrets installed at $ENV_DEST and $WRAPPER_DEST"
info "phase05 complete"
exit 0
