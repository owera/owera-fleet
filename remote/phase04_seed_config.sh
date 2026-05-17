#!/bin/bash
# phase04_seed_config.sh
#
# Phase 4 of the bootstrap pipeline. Runs ON A WORKER after being scp'd by
# the gateway. Drops the gateway's config.yaml (+ optionally skills/ and
# templates/ trees) into /Users/hermes/.hermes/, owned by the unprivileged
# 'hermes' user with mode 600 on config.yaml. Rewrites the gateway-pinned
# `tirith_path:` prefix from the gateway's home (where the gateway pinned
# /Users/<gateway>/.hermes/bin/tirith) to /Users/hermes/.hermes/ so that
# the worker's runtime PATH actually resolves it.
#
# The orchestrator is expected to scp the tarball bundle onto the worker
# first, at a path passed here via --config-bundle.
#
# Idempotent: re-running on a worker where config.yaml is already present
# with the right tirith_path is reported as no-change. (We don't try to
# do a full content diff between the bundled and on-disk config — that's
# what config-sync handles in steady state.)
#
# Bash 3.2 compatible (stock macOS /bin/bash).

set -uo pipefail

NODE_NAME=""
DRY_RUN="false"
CONFIG_BUNDLE=""
GATEWAY_HERMES_PREFIX="/Users/claw3/.hermes"   # default; overridable
HERMES_USER="hermes"
HERMES_HOME="/Users/hermes"
PHASE="phase04_seed_config"

usage() {
  cat <<EOF
Usage: phase04_seed_config.sh --config-bundle PATH [options]

Worker-side fragment that installs the gateway's hermes config under
/Users/hermes/.hermes/ and rewrites the tirith_path prefix.

Required:
  --config-bundle PATH         Path on the WORKER (typically /tmp/...) to a
                               tarball produced on the gateway by
                               'tar czf bundle -C \$HOME/.hermes config.yaml
                               [skills templates]'.

Options:
  --node NAME                  Node label for JSONL events. Default: \$(hostname -s)
  --gateway-hermes-prefix DIR  Gateway-side hermes-state prefix to rewrite
                               in tirith_path. Default: /Users/claw3/.hermes
  --hermes-user USER           Override the unprivileged user. Default: hermes
  --hermes-home PATH           Override the user home. Default: /Users/hermes
  --dry-run                    Probe only; no mutations.
  -h, --help                   Show this help.

Exit codes:
  0  Config seeded successfully.
  2  Config bundle missing or not readable.
  3  Extraction failed.
  4  tirith_path rewrite failed.
  5  Argument error.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --config-bundle)
      [ $# -ge 2 ] || { printf 'phase04: --config-bundle requires a value\n' >&2; exit 5; }
      CONFIG_BUNDLE="$2"; shift 2 ;;
    --node)
      [ $# -ge 2 ] || { printf 'phase04: --node requires a value\n' >&2; exit 5; }
      NODE_NAME="$2"; shift 2 ;;
    --gateway-hermes-prefix)
      [ $# -ge 2 ] || { printf 'phase04: --gateway-hermes-prefix requires a value\n' >&2; exit 5; }
      GATEWAY_HERMES_PREFIX="$2"; shift 2 ;;
    --hermes-user)
      [ $# -ge 2 ] || { printf 'phase04: --hermes-user requires a value\n' >&2; exit 5; }
      HERMES_USER="$2"; shift 2 ;;
    --hermes-home)
      [ $# -ge 2 ] || { printf 'phase04: --hermes-home requires a value\n' >&2; exit 5; }
      HERMES_HOME="$2"; shift 2 ;;
    --dry-run)
      DRY_RUN="true"; shift ;;
    -h|--help)
      usage; exit 0 ;;
    *)
      printf 'phase04: unknown option: %s\n' "$1" >&2
      usage >&2; exit 5 ;;
  esac
done

if [ -z "$CONFIG_BUNDLE" ]; then
  printf 'phase04: --config-bundle is required\n' >&2
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

info() { printf '[phase04] %s\n' "$*"; }

now_ms() {
  local s; s=$(date +%s)
  printf '%s' "$((s * 1000))"
}

# ---------------------------------------------------------------------------
# Actions
# ---------------------------------------------------------------------------

probe_bundle() {
  local start end dur
  start=$(now_ms)
  if [ ! -f "$CONFIG_BUNDLE" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "config_bundle_present" "error" "$dur" "missing: $CONFIG_BUNDLE"
    return 1
  fi
  if [ ! -r "$CONFIG_BUNDLE" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "config_bundle_present" "error" "$dur" "unreadable: $CONFIG_BUNDLE"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "config_bundle_present" "no-change" "$dur" "$CONFIG_BUNDLE"
  return 0
}

ensure_hermes_dir() {
  local start end dur d
  d="${HERMES_HOME}/.hermes"
  start=$(now_ms)
  if sudo -u "$HERMES_USER" test -d "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_hermes_dir" "no-change" "$dur" "$d exists"
    return 0
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_hermes_dir" "skipped" "$dur" "dry-run: would mkdir $d"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" mkdir -p "$d" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "ensure_hermes_dir" "error" "$dur" "mkdir -p $d failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "ensure_hermes_dir" "ok" "$dur" "created $d"
  return 0
}

extract_bundle() {
  local start end dur tmp_err rc tail dest
  dest="${HERMES_HOME}/.hermes"
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "extract_bundle" "skipped" "$dur" "dry-run: would tar -xzf $CONFIG_BUNDLE -C $dest"
    return 0
  fi
  tmp_err=$(mktemp -t phase04-tar.XXXXXX) || {
    log_action "extract_bundle" "error" 0 "mktemp failed"
    return 1
  }
  sudo -u "$HERMES_USER" tar -C "$dest" -xzf "$CONFIG_BUNDLE" 2> "$tmp_err"
  rc=$?
  end=$(now_ms); dur=$((end - start))
  tail=$(tail -c 1500 "$tmp_err" 2>/dev/null)
  rm -f "$tmp_err"
  if [ "$rc" -ne 0 ]; then
    log_action "extract_bundle" "error" "$dur" "rc=$rc; $tail"
    return 1
  fi
  log_action "extract_bundle" "ok" "$dur" "extracted into $dest"
  return 0
}

chmod_config() {
  local start end dur target
  target="${HERMES_HOME}/.hermes/config.yaml"
  start=$(now_ms)
  if ! sudo -u "$HERMES_USER" test -f "$target" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    if [ "$DRY_RUN" = "true" ]; then
      log_action "chmod_config" "skipped" "$dur" "dry-run: target missing"
      return 0
    fi
    log_action "chmod_config" "error" "$dur" "$target missing after extract"
    return 1
  fi
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "chmod_config" "skipped" "$dur" "dry-run: would chmod 600 $target"
    return 0
  fi
  if ! sudo -u "$HERMES_USER" chmod 600 "$target" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "chmod_config" "error" "$dur" "chmod 600 $target failed"
    return 1
  fi
  end=$(now_ms); dur=$((end - start))
  log_action "chmod_config" "ok" "$dur" "$target mode 600"
  return 0
}

rewrite_tirith_path() {
  local start end dur target rc grep_pat new_prefix
  target="${HERMES_HOME}/.hermes/config.yaml"
  new_prefix="${HERMES_HOME}/.hermes"
  grep_pat="tirith_path: ${GATEWAY_HERMES_PREFIX}/"
  start=$(now_ms)

  if ! sudo -u "$HERMES_USER" test -f "$target" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    if [ "$DRY_RUN" = "true" ]; then
      log_action "rewrite_tirith_path" "skipped" "$dur" "dry-run: target missing"
      return 0
    fi
    log_action "rewrite_tirith_path" "error" "$dur" "$target missing"
    return 1
  fi

  if ! sudo -u "$HERMES_USER" grep -qF "$grep_pat" "$target" 2>/dev/null; then
    end=$(now_ms); dur=$((end - start))
    log_action "rewrite_tirith_path" "no-change" "$dur" \
      "tirith_path already worker-relative (or relative-form retained)"
    return 0
  fi

  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "rewrite_tirith_path" "skipped" "$dur" \
      "dry-run: would rewrite ${GATEWAY_HERMES_PREFIX}/ -> ${new_prefix}/"
    return 0
  fi

  # Use sed -i.bak then drop the bak — macOS sed needs the suffix arg.
  sudo -u "$HERMES_USER" sed -i.bak \
    "s|tirith_path: ${GATEWAY_HERMES_PREFIX}/|tirith_path: ${new_prefix}/|" \
    "$target" 2>/dev/null
  rc=$?
  if [ "$rc" -ne 0 ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "rewrite_tirith_path" "error" "$dur" "sed rc=$rc"
    return 1
  fi
  sudo -u "$HERMES_USER" rm -f "${target}.bak" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "rewrite_tirith_path" "ok" "$dur" \
    "rewrote ${GATEWAY_HERMES_PREFIX}/ -> ${new_prefix}/"
  return 0
}

cleanup_bundle() {
  local start end dur
  start=$(now_ms)
  if [ "$DRY_RUN" = "true" ]; then
    end=$(now_ms); dur=$((end - start))
    log_action "cleanup_bundle" "skipped" "$dur" "dry-run: would rm $CONFIG_BUNDLE"
    return 0
  fi
  rm -f "$CONFIG_BUNDLE" 2>/dev/null || true
  end=$(now_ms); dur=$((end - start))
  log_action "cleanup_bundle" "ok" "$dur" "removed $CONFIG_BUNDLE"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

info "node=$NODE_NAME bundle=$CONFIG_BUNDLE gateway_prefix=$GATEWAY_HERMES_PREFIX dry_run=$DRY_RUN"

if ! probe_bundle; then
  log_action "summary" "error" 0 "config bundle not usable"
  exit 2
fi

if ! ensure_hermes_dir; then
  log_action "summary" "error" 0 "hermes home prep failed"
  exit 3
fi

if ! extract_bundle; then
  log_action "summary" "error" 0 "extract failed"
  exit 3
fi

if ! chmod_config; then
  log_action "summary" "error" 0 "chmod failed"
  exit 3
fi

if ! rewrite_tirith_path; then
  log_action "summary" "error" 0 "tirith_path rewrite failed"
  exit 4
fi

cleanup_bundle

log_action "summary" "ok" 0 "config.yaml seeded under ${HERMES_HOME}/.hermes/"
info "phase04 complete"
exit 0
