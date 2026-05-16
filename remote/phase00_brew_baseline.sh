#!/bin/bash
# phase00_brew_baseline.sh
#
# Phase 0 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Ensures Homebrew + the fixed essentials list (jq, coreutils,
# shell linter, gh, wget, ripgrep) are present, and that brew's shellenv
# is wired into ~/.local/bin/env so future delegated commands resolve
# `brew` on PATH.
#
# Self-contained: no gateway lib, no /Users/<gateway>/... paths. Only
# POSIX tools + curl (+ optionally /opt/homebrew/bin/brew or
# /usr/local/bin/brew once present).
#
# JSONL telemetry is emitted to **stderr** (one event per action) in the
# canonical Owera schema:
#   {ts, node, phase, action, result, duration_ms, stderr_tail}
# stdout is reserved for human-readable progress.
#
# Idempotent: re-running on an already-baseline'd worker emits a
# `no-change` line for every action and exits 0.
#
# The worker `hermes` user is non-admin by design (P4 lateral isolation).
# If Homebrew is missing this script CANNOT install it (the installer
# needs sudo to chown /opt/homebrew or /usr/local). In that case the
# script reports `error` for the `brew_present` action with a remediation
# hint in stderr_tail, and exits 2 so the gateway orchestrator can
# surface the admin-side install command. Once an admin has run the
# Homebrew installer once, every subsequent re-run of this script is a
# pure no-op.
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

# ---------------------------------------------------------------------------
# Defaults & args
# ---------------------------------------------------------------------------

DEFAULT_ESSENTIALS="jq coreutils shellcheck gh wget ripgrep"
ESSENTIALS="$DEFAULT_ESSENTIALS"
NODE_NAME=""
DRY_RUN="false"
PHASE="phase00_brew_baseline"

usage() {
  cat <<EOF
Usage: phase00_brew_baseline.sh [options]

Worker-side bootstrap fragment that ensures Homebrew + a fixed essentials
list are present, and that brew's shellenv is loaded by ~/.local/bin/env.

Options:
  --essentials "LIST"   Space-separated brew formula list.
                        Default: $DEFAULT_ESSENTIALS
  --node NAME           Override the node label emitted in JSONL events.
                        Default: \$(hostname -s)
  --dry-run             Probe only. No mutations. Every would-be mutation
                        is reported with result=skipped.
  -h, --help            Show this help.

Exit codes:
  0  All actions reported ok or no-change.
  2  Homebrew is missing and cannot be installed from this (non-admin)
     user — operator must run the official installer as an admin first.
  3  One or more essential installs failed.
  4  Argument / environment error.

JSONL telemetry goes to stderr; stdout is human-readable progress.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --essentials)
      [ $# -ge 2 ] || { printf 'phase00: --essentials requires a value\n' >&2; exit 4; }
      ESSENTIALS="$2"
      shift 2
      ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase00: --node requires a value\n' >&2; exit 4; }
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
      printf 'phase00: unknown option: %s\n' "$1" >&2
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
#
# Mirrors internal/log.Event. Written to stderr so stdout stays
# human-readable. RFC 3339 UTC timestamp; stderr_tail truncated to the
# last 2048 bytes (matches MaxStderrTailBytes in internal/log).
# ---------------------------------------------------------------------------

MAX_STDERR_TAIL=2048

_iso_now() {
  # `date -u +%FT%TZ` works on macOS 3.2 bash + GNU coreutils; second
  # precision is sufficient for human inspection, and matches the
  # behaviour of the gateway-side prototype.
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

_json_escape() {
  # Minimal JSON string escaping: backslash, doublequote, then collapse
  # newlines/tabs to single spaces. Sufficient because we never emit
  # control bytes < 0x20 from this script except newline/tab.
  local s="$1"
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=$(printf '%s' "$s" | tr '\n\r\t' '   ')
  printf '%s' "$s"
}

log_action() {
  # log_action <action> <result> <duration_ms> [<stderr_tail>]
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
  printf '[phase00] %s\n' "$*"
}

# ---------------------------------------------------------------------------
# Action helpers
# ---------------------------------------------------------------------------

now_ms() {
  # Bash 3.2 has no $EPOCHREALTIME. Second precision is fine for
  # bootstrap actions that range from milliseconds to minutes.
  local s
  s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# Map a brew formula to the binary that proves it's reachable on PATH.
# Most are 1:1 (e.g. jq -> jq); the exceptions live here.
binary_for() {
  case "$1" in
    coreutils) printf '%s' "gtimeout" ;;
    ripgrep)   printf '%s' "rg" ;;
    *)         printf '%s' "$1" ;;
  esac
}

# Locate brew. Sets BREW_PATH (empty if missing).
BREW_PATH=""
detect_brew() {
  local start end dur p
  start=$(now_ms)
  for p in /opt/homebrew/bin/brew /usr/local/bin/brew; do
    if [ -x "$p" ]; then
      BREW_PATH="$p"
      break
    fi
  done
  end=$(now_ms); dur=$((end - start))
  if [ -n "$BREW_PATH" ]; then
    local ver
    ver=$("$BREW_PATH" --version 2>/dev/null | head -1)
    log_action "brew_present" "no-change" "$dur" "$ver at $BREW_PATH"
    info "brew present: $BREW_PATH ($ver)"
    return 0
  fi
  log_action "brew_present" "error" "$dur" "Homebrew not found; non-admin user cannot install. Operator must run /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\" as an admin account on this Mac, then re-run phase00."
  info "brew MISSING — admin must install Homebrew before phase00 can complete"
  return 1
}

ensure_local_env_dir() {
  local start end dur d
  start=$(now_ms)
  d="$HOME/.local/bin"
  if [ -d "$d" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "local_bin_dir" "no-change" "$dur" "$d already exists"
    return 0
  fi
  end=$(now_ms); dur=$((end - start))
  if [ "$DRY_RUN" = "true" ]; then
    log_action "local_bin_dir" "skipped" "$dur" "dry-run: would mkdir -p $d"
    return 0
  fi
  if mkdir -p "$d" 2>/dev/null; then
    log_action "local_bin_dir" "ok" "$dur" "created $d"
  else
    log_action "local_bin_dir" "error" "$dur" "mkdir -p $d failed"
    return 1
  fi
}

# Append a brew shellenv eval to ~/.local/bin/env if it isn't already
# there. The file is sourced by delegated-command wrappers so brew lands
# on PATH for non-login shells.
ensure_shellenv() {
  local env_file start end dur
  env_file="$HOME/.local/bin/env"
  start=$(now_ms)
  if [ -f "$env_file" ] && grep -q "brew shellenv" "$env_file" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "shellenv_wired" "no-change" "$dur" "$env_file already evals brew shellenv"
    return 0
  fi
  end=$(now_ms); dur=$((end - start))
  if [ "$DRY_RUN" = "true" ]; then
    log_action "shellenv_wired" "skipped" "$dur" "dry-run: would append brew shellenv to $env_file"
    return 0
  fi
  if {
    printf '\n# Homebrew shellenv (added by phase00_brew_baseline.sh on %s)\n' "$(_iso_now)"
    # shellcheck disable=SC2016 # literal $(...) is what we want written.
    printf 'eval "$(%s shellenv)"\n' "$BREW_PATH"
  } >> "$env_file" 2>/dev/null; then
    log_action "shellenv_wired" "ok" "$dur" "appended brew shellenv to $env_file"
    return 0
  fi
  log_action "shellenv_wired" "error" "$dur" "could not append to $env_file"
  return 1
}

# brew_installed FORMULA — 0 if listed by `brew list --formula -1`.
# We materialize the list into a variable before grep'ing so we don't hit
# the SIGPIPE-on-early-grep-exit race that `set -o pipefail` can turn
# into a non-deterministic non-zero return code (observed in the
# idempotency test).
brew_installed() {
  local formula list
  formula="$1"
  list=$("$BREW_PATH" list --formula -1 2>/dev/null)
  printf '%s\n' "$list" | grep -qx "$formula"
}

install_essential() {
  local formula="$1" start end dur tmp_err rc tail
  start=$(now_ms)
  if brew_installed "$formula"; then
    end=$(now_ms); dur=$((end - start))
    log_action "install_$formula" "no-change" "$dur" "$formula already installed"
    return 0
  fi
  end=$(now_ms); dur=$((end - start))
  if [ "$DRY_RUN" = "true" ]; then
    log_action "install_$formula" "skipped" "$dur" "dry-run: would brew install $formula"
    return 0
  fi
  info "brew install $formula"
  tmp_err=$(mktemp -t phase00-brew.XXXXXX) || { log_action "install_$formula" "error" 0 "mktemp failed"; return 1; }
  start=$(now_ms)
  "$BREW_PATH" install "$formula" >/dev/null 2> "$tmp_err"
  rc=$?
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"
  if [ "$rc" -eq 0 ]; then
    log_action "install_$formula" "ok" "$dur" "installed $formula"
    return 0
  fi
  log_action "install_$formula" "error" "$dur" "rc=$rc; $tail"
  return 1
}

verify_essential() {
  local formula="$1" bin start end dur
  bin=$(binary_for "$formula")
  start=$(now_ms)
  # Pull brew shellenv into THIS shell so `command -v` sees the freshly
  # installed binary even though ~/.local/bin/env hasn't been re-sourced.
  eval "$("$BREW_PATH" shellenv)" 2>/dev/null
  end=$(now_ms); dur=$((end - start))
  if command -v "$bin" >/dev/null 2>&1; then
    log_action "verify_$formula" "no-change" "$dur" "$bin on PATH"
    return 0
  fi
  log_action "verify_$formula" "error" "$dur" "$bin NOT on PATH after install"
  return 1
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME essentials=\"$ESSENTIALS\" dry_run=$DRY_RUN"

OVERALL_RC=0

if ! detect_brew; then
  # No brew, no further work possible. Exit 2 — the orchestrator should
  # surface the admin-side install command to the operator.
  exit 2
fi

ensure_local_env_dir || OVERALL_RC=1
ensure_shellenv      || OVERALL_RC=1

INSTALL_FAILURES=0
for formula in $ESSENTIALS; do
  if ! install_essential "$formula"; then
    INSTALL_FAILURES=$((INSTALL_FAILURES + 1))
    continue
  fi
  if [ "$DRY_RUN" != "true" ]; then
    verify_essential "$formula" || INSTALL_FAILURES=$((INSTALL_FAILURES + 1))
  fi
done

if [ "$INSTALL_FAILURES" -gt 0 ]; then
  log_action "summary" "error" 0 "install_failures=$INSTALL_FAILURES essentials=\"$ESSENTIALS\""
  info "phase00 complete with $INSTALL_FAILURES failure(s)"
  exit 3
fi

if [ "$OVERALL_RC" -ne 0 ]; then
  log_action "summary" "warn" 0 "non-fatal helper(s) failed; essentials state is correct"
  info "phase00 complete with non-fatal warnings"
  exit 0
fi

log_action "summary" "ok" 0 "essentials=\"$ESSENTIALS\" all present"
info "phase00 complete"
exit 0
