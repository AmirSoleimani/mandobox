#!/usr/bin/env bash
# update-tools.sh — the versioned agent-tool update operation (docs/runbook.md → "Updating agent
# tools"). Pins the given CLI version(s) in tools.env, assembles a fresh golden image from the cached
# base (build.sh verifies the CLIs actually run), activates it, and appends to an audit log. Running
# VMs keep their pinned image; only new dispatches use the activated one — same safety as a full
# rebuild, in ~a minute. This is the single entry point a future management dashboard will call.
#
#   update-tools.sh [--claude VER] [--codex VER]
#
# With no args it just re-assembles at the currently pinned versions. Run as root on the fleet host.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT_DIR="${OUT_DIR:-/var/lib/fleet/images}"
BASE_DIR="${BASE_DIR:-/var/lib/fleet/base}"
TOOLS_ENV="${TOOLS_ENV:-$HERE/tools.env}"
SUPERVISOR_BIN="${SUPERVISOR_BIN:-$HERE/fc-supervisor}"
AUDIT="${AUDIT:-/var/lib/fleet/tool-updates.log}"

claude=""; codex=""
while [ $# -gt 0 ]; do
  case "$1" in
    --claude) claude="$2"; shift 2 ;;
    --codex)  codex="$2"; shift 2 ;;
    *) echo "usage: update-tools.sh [--claude VER] [--codex VER]" >&2; exit 2 ;;
  esac
done

# Update the manifest in place — the single source of truth for pinned CLI versions.
set_ver() {
  local k="$1" v="$2"; [ -n "$v" ] || return 0
  if grep -q "^${k}=" "$TOOLS_ENV" 2>/dev/null; then
    sed -i "s|^${k}=.*|${k}=${v}|" "$TOOLS_ENV"
  else
    printf '%s=%s\n' "$k" "$v" >> "$TOOLS_ENV"
  fi
}
set_ver CLAUDE_CODE_VERSION "$claude"
set_ver CODEX_VERSION "$codex"

export OUT_DIR BASE_DIR TOOLS_ENV SUPERVISOR_BIN
[ -f "$BASE_DIR/base.tar.zst" ] || { echo "update-tools: no cached base — building it (one-time, several minutes)…"; "$HERE/mkimage.sh" base; }

echo "update-tools: assembling a new image from the cached base…"
sha="$("$HERE/mkimage.sh" assemble | tail -1)"
[ -n "$sha" ] || { echo "update-tools: assemble produced no sha" >&2; exit 1; }
[ -f "$OUT_DIR/rootfs-${sha}.ext4" ] || { echo "update-tools: assembled image missing" >&2; exit 1; }

# assemble wrote the uncompressed .ext4 directly — activate it (running VMs keep their pinned image).
ln -sfn "rootfs-${sha}.ext4" "$OUT_DIR/current.ext4"
printf '%s\n' "$sha" > "$OUT_DIR/current.sha"

# shellcheck disable=SC1090
. "$TOOLS_ENV"
ts="$(date -u +%FT%TZ 2>/dev/null || echo unknown)"
printf '%s\tsha=%s\tclaude-code=%s\tcodex=%s\n' \
  "$ts" "$sha" "${CLAUDE_CODE_VERSION:-}" "${CODEX_VERSION:-}" >> "$AUDIT"
echo "update-tools: activated rootfs-${sha}.ext4 (claude-code=${CLAUDE_CODE_VERSION} codex=${CODEX_VERSION:-<none>})"
echo "$sha"
