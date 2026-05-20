#!/bin/bash
# phase02_create_user.sh
#
# Phase 2 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Creates the unprivileged 'hermes' user (UID 502, /bin/zsh,
# /Users/hermes home, primary group 'staff'/20, SSH access via
# com.apple.access_ssh) and refuses to proceed if a pre-existing
# 'hermes' is in the admin group (unless --allow-existing-admin-user).
#
# The user has NO password (dscl-created users are locked by default on
# macOS) and is not added to the admin group. The shell is /bin/zsh
# rather than a custom restricted shell to keep the user's interactive
# state convenient; lateral-movement isolation is enforced by the
# non-admin-group membership and Tirith's per-skill policy, not by shell
# pinning.
#
# Self-contained: all `dscl` / `createhomedir` / `dseditgroup` calls run
# locally via `sudo`. Idempotent: re-running on a host that already has
# the user reports no-change.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
ALLOW_EXISTING_ADMIN="false"
HERMES_USER="hermes"
HERMES_UID="502"
HERMES_GID="20"           # staff
HERMES_HOME="/Users/hermes"
HERMES_SHELL="/bin/zsh"
PHASE="phase02_create_user"

usage() {
  cat <<EOF
Usage: phase02_create_user.sh [options]

Worker-side fragment that creates the 'hermes' user as an unprivileged
account suitable for Hermes Agent workloads.

Options:
  --node NAME                    Node label for JSONL events.
                                 Default: \$(hostname -s)
  --uid N                        UID to assign. Default: 502
  --gid N                        Primary GID. Default: 20 (staff)
  --shell PATH                   Login shell. Default: /bin/zsh
  --allow-existing-admin-user    Allow a pre-existing 'hermes' user that is
                                 already in the admin group. Default: refuse.
  --dry-run                      Probe only. No mutations.
  -h, --help                     Show this help.

Exit codes:
  0  User created or already correctly configured.
  2  'hermes' already exists in admin group and --allow-existing-admin-user
     was not supplied.
  3  dscl/createhomedir failed.
  4  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --node)
      [ $# -ge 2 ] || { printf 'phase02: --node requires a value\n' >&2; exit 4; }
      NODE_NAME="$2"; shift 2 ;;
    --uid)
      [ $# -ge 2 ] || { printf 'phase02: --uid requires a value\n' >&2; exit 4; }
      HERMES_UID="$2"; shift 2 ;;
    --gid)
      [ $# -ge 2 ] || { printf 'phase02: --gid requires a value\n' >&2; exit 4; }
      HERMES_GID="$2"; shift 2 ;;
    --shell)
      [ $# -ge 2 ] || { printf 'phase02: --shell requires a value\n' >&2; exit 4; }
      HERMES_SHELL="$2"; shift 2 ;;
    --allow-existing-admin-user)
      ALLOW_EXISTING_ADMIN="true"; shift ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase02: unknown option: %s\n' "$1" >&2
      usage >&2
      exit 4
      ;;
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

info() { printf '[phase02] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Probes & mutations
# ---------------------------------------------------------------------------

user_exists() {
  local id_val
  id_val=$(dscl . -read "/Users/${HERMES_USER}" UniqueID 2>/dev/null | awk '{print $2}')
  [ -n "$id_val" ]
}

user_in_admin() {
  local n
  n=$(dscl . -read /Groups/admin GroupMembership 2>/dev/null \
        | tr ' ' '\n' | grep -cx "${HERMES_USER}" 2>/dev/null || echo 0)
  [ "$n" -ge 1 ] 2>/dev/null
}

ssh_group_member() {
  local n
  n=$(dseditgroup -o checkmember -m "${HERMES_USER}" com.apple.access_ssh 2>/dev/null \
        | grep -c "yes" 2>/dev/null || echo 0)
  [ "$n" -ge 1 ] 2>/dev/null
}

probe_existing() {
  local start end dur
  start=$(now_ms)
  if user_exists; then
    if user_in_admin; then
      end=$(now_ms); dur=$((end - start))
      if [ "$ALLOW_EXISTING_ADMIN" = "true" ]; then
        log_action "preflight_existing_user" "no-change" "$dur" \
          "${HERMES_USER} exists and is admin; --allow-existing-admin-user honoured"
        return 0
      fi
      log_action "preflight_existing_user" "error" "$dur" \
        "${HERMES_USER} is in admin group; refusing to proceed. Remove from admin or pass --allow-existing-admin-user."
      return 2
    fi
    end=$(now_ms); dur=$((end - start))
    log_action "preflight_existing_user" "no-change" "$dur" \
      "${HERMES_USER} exists, not in admin group"
    return 0
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "preflight_existing_user" "no-change" "$dur" \
    "${HERMES_USER} does not yet exist"
  return 0
}

# Read a single dscl attribute, handling both output forms macOS dscl emits:
#   single-line:  "KEY: value"
#   continuation: "KEY:\n value" (header line, then value indented on line 2,
#                  used when the value contains spaces or other shell-special
#                  characters)
# The old form `awk '{print $2}'` worked for single-line single-word values
# only — multi-word RealName ("Hermes Agent") tokenized to just "Agent",
# breaking idempotency on every re-run. Discovered on first live C1 against
# claw-staging.local on 2026-05-20.
read_dscl_attr() {
  # read_dscl_attr USER KEY
  local user="$1" key="$2"
  dscl . -read "/Users/${user}" "${key}" 2>/dev/null | awk -v k="${key}:" '
    NR==1 {
      if ($0 == k) { next }                  # continuation form: skip header
      sub("^" k " *", ""); print; exit       # single-line form: strip prefix
    }
    NR>=2 { sub(/^[[:space:]]+/, ""); print; exit }  # continuation value line
  '
}

# Apply each dscl attribute, skipping the underlying command when it
# already reports the desired value. Returns 0 on success, 1 on failure.
apply_dscl_attr() {
  # apply_dscl_attr KEY VALUE
  local key="$1" want="$2" cur start end dur
  start=$(now_ms)
  cur=$(read_dscl_attr "${HERMES_USER}" "$key")
  if [ "$cur" = "$want" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "dscl_${key}" "no-change" "$dur" "${key}=${want}"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "dscl_${key}" "skipped" "$dur" "dry-run: would set ${key}=${want}"
    return 0
  fi
  if [ -z "$cur" ]; then
    if ! sudo dscl . -create "/Users/${HERMES_USER}" "$key" "$want" >/dev/null 2>&1; then
      end=$(now_ms); dur=$((end - start))
      log_action "dscl_${key}" "error" "$dur" "dscl create ${key} failed"
      return 1
    fi
  else
    if ! sudo dscl . -change "/Users/${HERMES_USER}" "$key" "$cur" "$want" >/dev/null 2>&1; then
      end=$(now_ms); dur=$((end - start))
      log_action "dscl_${key}" "error" "$dur" "dscl change ${key} ${cur} -> ${want} failed"
      return 1
    fi
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "dscl_${key}" "ok" "$dur" "${key}=${want}"
  return 0
}

ensure_user_record() {
  local start end dur
  start=$(now_ms)
  if user_exists; then
    end=$(now_ms); dur=$((end - start))
    log_action "dscl_create_user" "no-change" "$dur" "${HERMES_USER} record exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "dscl_create_user" "skipped" "$dur" "dry-run: would dscl -create /Users/${HERMES_USER}"
    return 0
  fi
  if ! sudo dscl . -create "/Users/${HERMES_USER}" >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "dscl_create_user" "error" "$dur" "dscl -create failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "dscl_create_user" "ok" "$dur" "created /Users/${HERMES_USER}"
  return 0
}

ensure_homedir() {
  local start end dur
  start=$(now_ms)
  if [ -d "${HERMES_HOME}" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "createhomedir" "no-change" "$dur" "${HERMES_HOME} exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "createhomedir" "skipped" "$dur" "dry-run: would createhomedir -c -u ${HERMES_USER}"
    return 0
  fi
  if ! sudo createhomedir -c -u "${HERMES_USER}" >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "createhomedir" "error" "$dur" "createhomedir failed for ${HERMES_USER}"
    return 1
  fi
  if [ ! -d "${HERMES_HOME}" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "createhomedir" "error" "$dur" "${HERMES_HOME} still missing after createhomedir"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "createhomedir" "ok" "$dur" "created ${HERMES_HOME}"
  return 0
}

ensure_ssh_access_group() {
  local start end dur
  start=$(now_ms)
  if ssh_group_member; then
    end=$(now_ms); dur=$((end - start))
    log_action "ssh_access_group" "no-change" "$dur" \
      "${HERMES_USER} already in com.apple.access_ssh"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ssh_access_group" "skipped" "$dur" \
      "dry-run: would dseditgroup -a ${HERMES_USER} com.apple.access_ssh"
    return 0
  fi
  if ! sudo dseditgroup -o edit -a "${HERMES_USER}" -t user com.apple.access_ssh >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "ssh_access_group" "error" "$dur" "dseditgroup add failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "ssh_access_group" "ok" "$dur" \
    "added ${HERMES_USER} to com.apple.access_ssh"
  return 0
}

probe_password_locked() {
  # On macOS, `dscl . -create /Users/<u>` without a `passwd` line yields a
  # locked account. We confirm the AuthenticationAuthority does NOT list a
  # ShadowHash without a Disabled tag. This is a best-effort probe — read
  # access requires sudo on some macOS versions.
  local start end dur out
  start=$(now_ms)
  out=$(sudo dscl . -read "/Users/${HERMES_USER}" AuthenticationAuthority 2>/dev/null || true)
  end=$(now_ms); dur=$((end - start))
  if [ -z "$out" ] || printf '%s' "$out" | grep -q "DisabledUser"; then
    log_action "password_locked" "no-change" "$dur" "no active ShadowHash"
    return 0
  fi
  # We don't fail bootstrap on this — log warn and continue.
  log_action "password_locked" "warn" "$dur" "AuthenticationAuthority present; review on host"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME user=$HERMES_USER uid=$HERMES_UID gid=$HERMES_GID shell=$HERMES_SHELL dry_run=$DRY_RUN"

if ! probe_existing; then
  log_action "summary" "error" 0 "preflight rejected existing admin user"
  exit 2
fi

if ! ensure_user_record; then
  log_action "summary" "error" 0 "user record create failed"
  exit 3
fi

# Set every attribute the prototype's phase_create_user pinned.
FAIL=0
apply_dscl_attr "UserShell"         "$HERMES_SHELL" || FAIL=1
apply_dscl_attr "RealName"          "Hermes Agent"  || FAIL=1
apply_dscl_attr "UniqueID"          "$HERMES_UID"   || FAIL=1
apply_dscl_attr "PrimaryGroupID"    "$HERMES_GID"   || FAIL=1
apply_dscl_attr "NFSHomeDirectory"  "$HERMES_HOME"  || FAIL=1

if [ "$FAIL" -ne 0 ]; then
  log_action "summary" "error" 0 "one or more dscl attribute applies failed"
  exit 3
fi

ensure_homedir         || { log_action "summary" "error" 0 "homedir creation failed"; exit 3; }
ensure_ssh_access_group || { log_action "summary" "error" 0 "ssh access group failed"; exit 3; }
probe_password_locked

log_action "summary" "ok" 0 "${HERMES_USER} provisioned (uid=${HERMES_UID}, home=${HERMES_HOME})"
info "phase02 complete"
exit 0
