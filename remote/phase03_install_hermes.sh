#!/bin/bash
# phase03_install_hermes.sh
#
# Phase 3 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Installs the Hermes Agent binary at the pinned version
# under the unprivileged 'hermes' user, and seeds /Users/hermes/.hermes/
# PINNED_VERSION for later drift detection by fleet-health-snapshot.
#
# Idempotent: if `hermes version` already reports the pinned version,
# the installer is not re-run.
#
# Self-contained: no gateway lib paths. The pinned version is supplied
# via --pinned-version (REQUIRED) so the gateway has explicit control over
# what's installed and we don't have to ship gateway state to the worker.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
PINNED_VERSION=""
INSTALL_TAG=""
INSTALL_URL_DEFAULT="https://hermes-agent.nousresearch.com/install.sh"
INSTALL_URL="$INSTALL_URL_DEFAULT"
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PHASE="phase03_install_hermes"
PRINT_INSTALL_CMD="false"

usage() {
  cat <<EOF
Usage: phase03_install_hermes.sh --pinned-version TAG [options]

Worker-side fragment that installs Hermes Agent at the pinned version
under the unprivileged 'hermes' user.

Required:
  --pinned-version TAG     Hermes version tag to install (e.g. v0.13.0).

Options:
  --install-tag TAG        Git tag (CalVer, e.g. v2026.5.7) to pass to the
                           Nous installer via --branch. The Nous installer
                           IGNORES HERMES_VERSION env vars — pinning happens
                           via --branch only. When set, --pinned-version is
                           used solely for the post-install verification
                           assertion; the actual install ref comes from this
                           flag. When unset, the installer defaults to the
                           main branch (whatever upstream is currently
                           tagged latest at curl time). The fleetctl runner
                           resolves this from --pinned-version via the
                           internal/hermesrelease package.
  --node NAME              Node label for JSONL events. Default: \$(hostname -s)
  --install-url URL        Override the installer URL.
                           Default: $INSTALL_URL_DEFAULT
  --hermes-user USER       Override the unprivileged user. Default: hermes
  --hermes-home PATH       Override the user home. Default: /Users/hermes
  --dry-run                Probe only; no installer pipeline executes.
  --print-install-cmd      Print the curl|bash command that WOULD run and exit 0.
                           Does not invoke sudo / write the seed file. Used by
                           live-verification harnesses to confirm pinning.
  -h, --help               Show this help.

Exit codes:
  0  Hermes installed at the pinned version (or already was).
  2  Xcode CLT missing (installer would fail at git-clone time).
  3  Installer pipeline failed.
  4  hermes binary not on PATH after install.
  5  Pinned-version mismatch after install.
  6  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --pinned-version)
      [ $# -ge 2 ] || { printf 'phase03: --pinned-version requires a value\n' >&2; exit 6; }
      PINNED_VERSION="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase03: --node requires a value\n' >&2; exit 6; }
      NODE_NAME="$2"; shift 2 ;;
    --install-url)
      [ $# -ge 2 ] || { printf 'phase03: --install-url requires a value\n' >&2; exit 6; }
      INSTALL_URL="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase03: --hermes-user requires a value\n' >&2; exit 6; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase03: --hermes-home requires a value\n' >&2; exit 6; }
      HERMES_HOME="$2"; shift 2 ;;
    --install-tag)
      [ $# -ge 2 ] || { printf 'phase03: --install-tag requires a value\n' >&2; exit 6; }
      INSTALL_TAG="$2"; shift 2 ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    --print-install-cmd)
      PRINT_INSTALL_CMD="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase03: unknown option: %s\n' "$1" >&2
      usage >&2; exit 6 ;;
  esac
done

if [ -z "$PINNED_VERSION" ]; then
  printf 'phase03: --pinned-version is required\n' >&2
  usage >&2
  exit 6
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

info() { printf '[phase03] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Probes & mutations
# ---------------------------------------------------------------------------

probe_xcode_clt() {
  local start end dur clt_path
  start=$(now_ms)
  clt_path=$(xcode-select -p 2>/dev/null || true)
  end=$(now_ms); dur=$((end - start))
  if [ -n "$clt_path" ] && [ -d "$clt_path" ]; then
    log_action "xcode_clt" "no-change" "$dur" "CLT at $clt_path"
    return 0
  fi
  log_action "xcode_clt" "error" "$dur" \
    "Xcode CLT missing; required by the Hermes installer's git clone. Run 'xcode-select --install' on the node first."
  return 1
}

current_hermes_version() {
  sudo -u "$HERMES_USER" -i sh -c \
    '[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env"; hermes version 2>/dev/null | head -1' \
    2>/dev/null || true
}

probe_already_pinned() {
  local start end dur v
  start=$(now_ms)
  v=$(current_hermes_version)
  end=$(now_ms); dur=$((end - start))
  if printf '%s' "$v" | grep -qF "$PINNED_VERSION"; then
    log_action "hermes_version_pinned" "no-change" "$dur" "$v"
    return 0
  fi
  log_action "hermes_version_pinned" "no-change" "$dur" "current=$v target=$PINNED_VERSION (will install)"
  return 1
}

build_installer_cmd() {
  # Print the curl|bash one-liner that run_installer would execute. The
  # Nous installer ignores HERMES_VERSION entirely (zero matches in its
  # current install.sh); pinning happens via `--branch <git-tag>`. So
  # when --install-tag is supplied we append --branch; when it's empty
  # the installer falls back to whatever main resolves to at curl time.
  # We still set HERMES_VERSION as a hint for any future installer that
  # might honour it — harmless today, and makes the intent visible in
  # `ps`.
  local branch_arg=""
  if [ -n "$INSTALL_TAG" ]; then
    branch_arg=" --branch $INSTALL_TAG"
  fi
  printf 'curl -fsSL %s | HERMES_VERSION=%s bash -s -- --skip-setup%s' \
    "$INSTALL_URL" "$PINNED_VERSION" "$branch_arg"
}

run_installer() {
  local start end dur tmp_err rc tail installer remote_cmd
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "hermes_installer" "skipped" "$dur" "dry-run: would $(build_installer_cmd)"
    return 0
  fi

  installer=$(build_installer_cmd)
  # shellcheck disable=SC2016  # literal $HOME / $PATH must reach the remote sh.
  remote_cmd='set -e
'"$installer"'
[ -f "$HOME/.local/bin/env" ] && . "$HOME/.local/bin/env"
if ! command -v hermes >/dev/null 2>&1; then
  echo "FATAL: hermes binary not on PATH after install (HOME=$HOME, PATH=$PATH)" >&2
  ls -la "$HOME/.local/bin" 2>&1 >&2 || true
  exit 1
fi
BIN=$(command -v hermes)
xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true
'

  tmp_err=$(mktemp -t phase03-install.XXXXXX) || {
    log_action "hermes_installer" "error" 0 "mktemp failed"
    return 1
  }

  # Earlier form used `sudo -u hermes -i sh -c "$remote_cmd"`, but sudo's
  # -i shell-escapes the command argument, which collapsed the embedded
  # newlines into a single line — `set -e` ran into `curl ...` producing
  # `set -ecurl ...` and a syntax error. Fix: stage the multi-line script
  # as a temp file readable by hermes and invoke it. Discovered on first
  # live C1 run against claw-staging.local on 2026-05-20.
  tmp_script=$(mktemp -t phase03-installXXXXXX.sh) || {
    rm -f "$tmp_err"
    log_action "hermes_installer" "error" 0 "mktemp (script) failed"
    return 1
  }
  printf '%s' "$remote_cmd" > "$tmp_script"
  chmod 755 "$tmp_script"

  sudo -u "$HERMES_USER" -i bash "$tmp_script" >/dev/null 2> "$tmp_err"
  rc=$?
  rm -f "$tmp_script"
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"

  if [ "$rc" -ne 0 ]; then
    log_action "hermes_installer" "error" "$dur" "rc=$rc; $tail"
    return 1
  fi
  log_action "hermes_installer" "ok" "$dur" "installed via curl|bash at $PINNED_VERSION"
  return 0
}

probe_hermes_on_path() {
  local start end dur v
  start=$(now_ms)
  v=$(current_hermes_version)
  end=$(now_ms); dur=$((end - start))
  if [ -z "$v" ]; then
    if [ "$DRY_RUN" = "true" ]; then
      log_action "hermes_on_path" "skipped" "$dur" "dry-run: probe deferred"
      return 0
    fi
    log_action "hermes_on_path" "error" "$dur" "hermes binary not invokable as $HERMES_USER"
    return 1
  fi
  log_action "hermes_on_path" "no-change" "$dur" "$v"
  return 0
}

verify_pinned() {
  local start end dur v
  start=$(now_ms)
  v=$(current_hermes_version)
  end=$(now_ms); dur=$((end - start))
  if [ "$DRY_RUN" = "true" ]; then
    log_action "verify_pinned" "skipped" "$dur" "dry-run: verify deferred"
    return 0
  fi
  if printf '%s' "$v" | grep -qF "$PINNED_VERSION"; then
    log_action "verify_pinned" "no-change" "$dur" "$v"
    return 0
  fi
  log_action "verify_pinned" "error" "$dur" "want=$PINNED_VERSION got=$v"
  return 1
}

seed_pinned_version_file() {
  local start end dur target
  target="${HERMES_HOME}/.hermes/PINNED_VERSION"
  start=$(now_ms)
  if sudo -u "$HERMES_USER" sh -c "test -f '$target' && grep -qF '$PINNED_VERSION' '$target'" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "seed_pinned_version" "no-change" "$dur" "$target already records $PINNED_VERSION"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "seed_pinned_version" "skipped" "$dur" "dry-run: would write $target"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" mkdir -p "${HERMES_HOME}/.hermes" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "seed_pinned_version" "error" "$dur" "mkdir ${HERMES_HOME}/.hermes failed"
    return 1
  fi
  if ! printf '%s\n' "$PINNED_VERSION" | sudo -u "$HERMES_USER" tee "$target" >/dev/null 2>&1; then
    end=$(now_ms); dur=$((end - start))
    log_action "seed_pinned_version" "error" "$dur" "tee $target failed"
    return 1
  fi
  sudo -u "$HERMES_USER" chmod 644 "$target" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "seed_pinned_version" "ok" "$dur" "wrote $target"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME pinned=$PINNED_VERSION tag=${INSTALL_TAG:-<unpinned-main>} user=$HERMES_USER dry_run=$DRY_RUN"

if [ "$PRINT_INSTALL_CMD" = "true" ]; then
  build_installer_cmd
  printf '\n'
  exit 0
fi

if ! probe_xcode_clt; then
  log_action "summary" "error" 0 "Xcode CLT precondition failed"
  exit 2
fi

NEED_INSTALL=0
probe_already_pinned || NEED_INSTALL=1

if [ "$NEED_INSTALL" -eq 1 ]; then
  if ! run_installer; then
    log_action "summary" "error" 0 "installer pipeline failed"
    exit 3
  fi
fi

if ! probe_hermes_on_path; then
  log_action "summary" "error" 0 "hermes binary not on PATH for $HERMES_USER"
  exit 4
fi

if ! verify_pinned; then
  log_action "summary" "error" 0 "pinned-version verification failed"
  exit 5
fi

if ! seed_pinned_version_file; then
  log_action "summary" "error" 0 "failed to seed PINNED_VERSION file"
  exit 3
fi

log_action "summary" "ok" 0 "hermes installed at $PINNED_VERSION; PINNED_VERSION seeded"
info "phase03 complete"
exit 0
