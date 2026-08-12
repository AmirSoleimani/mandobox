#!/usr/bin/env bash
# build.sh — split, cached golden-image build (docs/runbook.md → "Updating agent tools").
#
#   build.sh base       Build the heavy, stable base rootfs (Debian + node + gh + vscode-cli + go +
#                       golangci-lint + ruff) once and cache it as a tarball. Rebuild only when the
#                       base toolchain versions change (rare).
#   build.sh assemble   Fast path: extract the cached base, npm-install the pinned agent CLIs
#                       (claude-code, codex from tools.env), verify they run, drop in fc-supervisor,
#                       and emit a content-addressed rootfs-<sha>.ext4.zst. No mmdebstrap, no big
#                       downloads — this is what a CLI bump rebuilds.
#
# The base carries node, so `assemble` can npm-install the CLIs against it; the CLIs are versioned in
# the same atomic image as their runtime (no cross-artifact skew — 2b, docs/configuration decision).
# Requires: mmdebstrap, mke2fs, zstd, curl, tar; a prebuilt fc-supervisor (SUPERVISOR_BIN) for assemble.
set -euo pipefail

CMD="${1:-assemble}"
OUT_DIR="${OUT_DIR:-/var/lib/fleet/images}"
BASE_DIR="${BASE_DIR:-/var/lib/fleet/base}"
BASE_TAR="$BASE_DIR/base.tar.zst"
SIZE_MB="${SIZE_MB:-3072}"  # ~2–3GB (headroom for the baked-in Chromium/Playwright)
HERE="$(cd "$(dirname "$0")" && pwd)"

# Distro match (a Debian rootfs on an Ubuntu host fails apt signature verification, and vice versa).
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-debian}" in
  ubuntu) SUITE="${SUITE:-${VERSION_CODENAME:-jammy}}"; MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"; COMPONENTS="${COMPONENTS:-main,universe}" ;;
  *)      SUITE="${SUITE:-bookworm}"; MIRROR="${MIRROR:-http://deb.debian.org/debian}"; COMPONENTS="${COMPONENTS:-main}" ;;
esac

# Base toolchain versions (stable — a change means rebuilding the base).
NODE_MAJOR="${NODE_MAJOR:-22}"
PLAYWRIGHT_VERSION="${PLAYWRIGHT_VERSION:-1.62.1}"  # headless browser for visual self-verification
GH_VERSION="${GH_VERSION:-2.96.0}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-2.12.2}"
GO_VERSION="${GO_VERSION:-1.25.12}"
# sha256 of the pinned Go toolchain tarball — verified before install. Update with GO_VERSION.
GO_SHA256="${GO_SHA256:-234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1}"

# Agent-CLI versions (fast-changing — the single source of truth is tools.env, overridable by env).
if [ -f "${TOOLS_ENV:-$HERE/tools.env}" ]; then
  # shellcheck disable=SC1090
  . "${TOOLS_ENV:-$HERE/tools.env}"
fi
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-2.1.220}"
CODEX_VERSION="${CODEX_VERSION:-0.147.0}"   # pinned fallback if tools.env is absent (never "latest")

need() { for t in "$@"; do command -v "$t" >/dev/null || { echo "build: missing tool: $t" >&2; exit 2; }; done; }

build_base() {
  need mmdebstrap mke2fs tar zstd curl
  local work rootfs mounted="" fs
  work="$(mktemp -d)"; rootfs="$work/rootfs"
  trap 'for m in $mounted; do umount -l "$m" 2>/dev/null || true; done; rm -rf "$work"' RETURN

  echo "build: base — mmdebstrap (${ID:-debian} ${SUITE})"
  mmdebstrap --variant=minbase --components="$COMPONENTS" \
    --include=apt,ca-certificates,curl,gnupg,git,jq,ripgrep,fd-find,python3,python3-venv,python3-pip,openssh-client,less,procps,iproute2,e2fsprogs \
    "$SUITE" "$rootfs" "$MIRROR"

  cp /etc/resolv.conf "$rootfs/etc/resolv.conf"
  for fs in proc sys dev; do mount --bind "/$fs" "$rootfs/$fs"; mounted="$rootfs/$fs $mounted"; done

  # mando-shot (visual self-verification) is a repo-local asset; drop it in before the chroot installs
  # its Playwright/Chromium runtime around it.
  install -D -m 0755 "$HERE/assets/mando-shot.js" "$rootfs/opt/mando-shot/mando-shot.js"

  cat >"$rootfs/tmp/base.sh" <<INSTALL
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
ln -sf "\$(command -v fdfind)" /usr/local/bin/fd
curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
apt-get install -y --no-install-recommends nodejs
# Headless browser (Playwright + Chromium) for visual self-verification (docs/preview.md): the agent
# screenshots its own running change and reads the PNG back. In-guest vs localhost only — no egress.
( cd /opt/mando-shot && npm init -y >/dev/null && npm install "playwright@${PLAYWRIGHT_VERSION}" )
PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright /opt/mando-shot/node_modules/.bin/playwright install --with-deps chromium
ln -sf /opt/mando-shot/mando-shot.js /usr/local/bin/mando-shot
curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz"
tar -xzf /tmp/gh.tgz -C /tmp
install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh
curl -fsSL -o /tmp/vscode-cli.tgz "https://update.code.visualstudio.com/latest/cli-linux-x64/stable"
tar -xzf /tmp/vscode-cli.tgz -C /usr/local/bin
test -x /usr/local/bin/code
curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - || { echo "build: go toolchain checksum mismatch" >&2; exit 2; }
tar -C /usr/local -xzf /tmp/go.tgz
curl -fsSL -o /tmp/gcl.tgz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz"
tar -xzf /tmp/gcl.tgz -C /tmp
install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" /usr/local/bin/
python3 -m venv /opt/pytools
/opt/pytools/bin/pip install --no-cache-dir ruff
ln -sf /opt/pytools/bin/ruff /usr/local/bin/ruff
apt-get clean; rm -rf /var/lib/apt/lists/* /tmp/*
INSTALL
  echo "build: base — install stable toolchain"
  chroot "$rootfs" /bin/bash /tmp/base.sh
  rm -f "$rootfs/tmp/base.sh"

  cat >>"$rootfs/etc/environment" <<'ENVV'
GOMODCACHE=/workspace/.cache/go/mod
GOCACHE=/workspace/.cache/go/build
npm_config_cache=/workspace/.cache/npm
PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/sbin
ENVV
  printf 'node_major=%s\nplaywright=%s\ngh=%s\ngo=%s\ngolangci-lint=%s\n' \
    "$NODE_MAJOR" "$PLAYWRIGHT_VERSION" "$GH_VERSION" "$GO_VERSION" "$GOLANGCI_LINT_VERSION" > "$rootfs/etc/fleet-base-versions"

  for m in $mounted; do umount -l "$m" 2>/dev/null || true; done; mounted=""
  rm -f "$rootfs/etc/resolv.conf"

  mkdir -p "$BASE_DIR"
  echo "build: base — pack $BASE_TAR"
  tar -C "$rootfs" -cf - . | zstd -q -19 -o "$BASE_TAR"
  echo "build: base ready ($BASE_TAR)"
}

assemble() {
  need mke2fs tar zstd
  : "${SUPERVISOR_BIN:?set SUPERVISOR_BIN to the prebuilt fc-supervisor}"
  [ -x "$SUPERVISOR_BIN" ] || { echo "build: $SUPERVISOR_BIN not executable" >&2; exit 2; }
  [ -f "$BASE_TAR" ] || { echo "build: no cached base ($BASE_TAR) — run 'build.sh base' first" >&2; exit 3; }

  local work rootfs mounted="" fs
  work="$(mktemp -d)"; rootfs="$work/rootfs"; mkdir -p "$rootfs"
  trap 'for m in $mounted; do umount -l "$m" 2>/dev/null || true; done; rm -rf "$work"' RETURN

  echo "build: assemble — extract base"
  zstd -dc "$BASE_TAR" | tar -C "$rootfs" -xf -
  cp /etc/resolv.conf "$rootfs/etc/resolv.conf"
  for fs in proc sys dev; do mount --bind "/$fs" "$rootfs/$fs"; mounted="$rootfs/$fs $mounted"; done

  cat >"$rootfs/tmp/tools.sh" <<INSTALL
set -euo pipefail
npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
claude --version   # verify the pinned CLI actually runs before we ship it
if [ -n "${CODEX_VERSION}" ]; then
  npm install -g "@openai/codex@${CODEX_VERSION}"
  codex --version
fi
npm cache clean --force
rm -rf /tmp/*
INSTALL
  echo "build: assemble — install + verify agent CLIs (claude=${CLAUDE_CODE_VERSION} codex=${CODEX_VERSION:-<none>})"
  chroot "$rootfs" /bin/bash /tmp/tools.sh
  rm -f "$rootfs/tmp/tools.sh"

  install -m 0755 "$SUPERVISOR_BIN" "$rootfs/sbin/fc-supervisor"
  ln -sf /sbin/fc-supervisor "$rootfs/sbin/init"
  printf 'claude-code=%s\ncodex=%s\n' "$CLAUDE_CODE_VERSION" "${CODEX_VERSION:-}" >> "$rootfs/etc/fleet-image-versions"
  cat "$rootfs/etc/fleet-base-versions" >> "$rootfs/etc/fleet-image-versions" 2>/dev/null || true

  for m in $mounted; do umount -l "$m" 2>/dev/null || true; done; mounted=""
  rm -f "$rootfs/etc/resolv.conf"

  echo "build: assemble — mke2fs (${SIZE_MB} MiB)"
  mkdir -p "$OUT_DIR"
  local tmp="$OUT_DIR/.assemble-$$.ext4"
  mke2fs -q -t ext4 -d "$rootfs" -b 4096 "$tmp" "${SIZE_MB}M"
  local sha; sha="$(sha256sum "$tmp" | cut -d' ' -f1)"
  # On-box build: emit the uncompressed ext4 that mando-agent launches from directly — no zstd
  # round-trip (it's the slow part, and there's nothing to transfer). CI (build.sh) still ships .zst.
  mv -f "$tmp" "$OUT_DIR/rootfs-${sha}.ext4"
  printf '%s\n' "$sha" > "$OUT_DIR/rootfs-${sha}.sha256"
  echo "build: assemble wrote $OUT_DIR/rootfs-${sha}.ext4"
  echo "$sha"
}

case "$CMD" in
  base)     build_base ;;
  assemble) assemble ;;
  *) echo "usage: build.sh {base|assemble}" >&2; exit 2 ;;
esac
