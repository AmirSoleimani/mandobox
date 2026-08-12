#!/usr/bin/env bash
# build-mmdebstrap.sh — build the golden rootfs WITHOUT Docker, on any Debian/Ubuntu box
# (the fleet host itself works). Output is content-addressed rootfs-<sha>.ext4.zst.
#
# Requires: mmdebstrap, debian-archive-keyring, e2fsprogs (mke2fs), zstd, curl, and a
# prebuilt fc-supervisor binary (make dist -> bin/fc-supervisor). Run as root.
#
# Versions mirror image/Dockerfile — keep them in sync.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
SUPERVISOR_BIN="${SUPERVISOR_BIN:?set SUPERVISOR_BIN to the prebuilt fc-supervisor binary}"
OUT_DIR="${OUT_DIR:-/var/lib/fleet/images}"
SIZE_MB="${SIZE_MB:-3072}"  # ~2–3GB (headroom for the baked-in Chromium/Playwright)
# Build a rootfs matching the HOST distro so mmdebstrap uses the host's own apt keyring
# (a Debian rootfs on an Ubuntu host fails signature verification — Ubuntu's apt does not
# trust Debian's signing keys, and vice versa). The guest being Ubuntu vs Debian makes no
# functional difference; it just runs Node + Claude Code + fc-supervisor.
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-debian}" in
  ubuntu)
    SUITE="${SUITE:-${VERSION_CODENAME:-jammy}}"
    MIRROR="${MIRROR:-http://archive.ubuntu.com/ubuntu}"
    COMPONENTS="${COMPONENTS:-main,universe}" # ripgrep/fd-find live in universe on Ubuntu
    ;;
  *)
    SUITE="${SUITE:-bookworm}"
    MIRROR="${MIRROR:-http://deb.debian.org/debian}"
    COMPONENTS="${COMPONENTS:-main}"
    ;;
esac

NODE_MAJOR="${NODE_MAJOR:-22}"
PLAYWRIGHT_VERSION="${PLAYWRIGHT_VERSION:-1.62.1}"  # headless browser for visual self-verification
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-2.1.220}"
# Codex CLI is baked into the image by default so switching harnesses is a pure config toggle
# (mandobox.yml agents_allowed) with no image rebuild — "available, not enabled". Set CODEX_VERSION=""
# to omit it and slim the image. Pin a concrete version once you've verified one.
CODEX_VERSION="${CODEX_VERSION:-0.147.0}"   # pinned — never "latest" (reproducible, reviewed bumps)
GH_VERSION="${GH_VERSION:-2.96.0}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-2.12.2}"
GO_VERSION="${GO_VERSION:-1.25.12}"
# sha256 of the pinned Go toolchain tarball — verified before install (a poisoned toolchain would
# taint every guest build). Update alongside GO_VERSION (value from https://go.dev/dl/).
GO_SHA256="${GO_SHA256:-234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1}"

[ -x "$SUPERVISOR_BIN" ] || { echo "build: $SUPERVISOR_BIN not found/executable" >&2; exit 2; }
for t in mmdebstrap mke2fs zstd curl; do
  command -v "$t" >/dev/null || { echo "build: missing tool: $t" >&2; exit 2; }
done

mkdir -p "$OUT_DIR"
WORK="$(mktemp -d)"
ROOTFS="$WORK/rootfs"
mounted=""
cleanup() {
  for m in $mounted; do umount -l "$m" 2>/dev/null || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

echo "build: mmdebstrap base (${ID:-debian} ${SUITE}, components ${COMPONENTS})"
# minbase omits apt (it is priority:important, not required); the chroot installers need it.
mmdebstrap --variant=minbase --components="$COMPONENTS" \
  --include=apt,ca-certificates,curl,gnupg,git,jq,ripgrep,fd-find,python3,python3-venv,python3-pip,openssh-client,less,procps,iproute2,e2fsprogs \
  "$SUITE" "$ROOTFS" "$MIRROR"

# --- run installers inside the rootfs (needs network + pseudo-filesystems) ---
cp /etc/resolv.conf "$ROOTFS/etc/resolv.conf"
for fs in proc sys dev; do
  mount --bind "/$fs" "$ROOTFS/$fs"
  mounted="$ROOTFS/$fs $mounted"
done

# mando-shot (visual self-verification) is a repo-local asset; drop it in before the chroot installs
# its Playwright/Chromium runtime around it.
install -D -m 0755 "$HERE/assets/mando-shot.js" "$ROOTFS/opt/mando-shot/mando-shot.js"

cat >"$ROOTFS/tmp/install.sh" <<INSTALL
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
ln -sf "\$(command -v fdfind)" /usr/local/bin/fd

# Node LTS + pinned Claude Code CLI.
curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
apt-get install -y --no-install-recommends nodejs
npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"

# Headless browser (Playwright + Chromium) for visual self-verification (docs/preview.md): the agent
# screenshots its own running change and reads the PNG back. In-guest vs localhost only — no egress.
( cd /opt/mando-shot && npm init -y >/dev/null && npm install "playwright@${PLAYWRIGHT_VERSION}" )
PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright /opt/mando-shot/node_modules/.bin/playwright install --with-deps chromium
ln -sf /opt/mando-shot/mando-shot.js /usr/local/bin/mando-shot
# Optional: OpenAI Codex CLI as a second harness (only when CODEX_VERSION is set). Non-fatal so a
# bad/unavailable version never breaks the image build.
if [ -n "${CODEX_VERSION}" ]; then
  npm install -g "@openai/codex@${CODEX_VERSION}" || echo "WARN: codex install failed (optional harness)"
fi
npm cache clean --force

# GitHub CLI.
curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz"
tar -xzf /tmp/gh.tgz -C /tmp
install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh

# VS Code CLI — headless 'code tunnel' lets a trusted operator open a browser VS Code into a live
# VM to inspect/edit (human attach). The standalone-CLI archive is a single 'code' binary.
curl -fsSL -o /tmp/vscode-cli.tgz "https://update.code.visualstudio.com/latest/cli-linux-x64/stable"
tar -xzf /tmp/vscode-cli.tgz -C /usr/local/bin
test -x /usr/local/bin/code || { echo "vscode cli: 'code' not extracted"; exit 1; }

# Go toolchain + golangci-lint.
curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - || { echo "build: go toolchain checksum mismatch" >&2; exit 2; }
tar -C /usr/local -xzf /tmp/go.tgz
curl -fsSL -o /tmp/gcl.tgz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz"
tar -xzf /tmp/gcl.tgz -C /tmp
install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" /usr/local/bin/

# Python linter/formatter.
python3 -m venv /opt/pytools
/opt/pytools/bin/pip install --no-cache-dir ruff
ln -sf /opt/pytools/bin/ruff /usr/local/bin/ruff

# Provenance + cleanup.
printf 'claude-code=%s\nnode_major=%s\nplaywright=%s\ngh=%s\ngo=%s\ngolangci-lint=%s\n' \
  "${CLAUDE_CODE_VERSION}" "${NODE_MAJOR}" "${PLAYWRIGHT_VERSION}" "${GH_VERSION}" "${GO_VERSION}" "${GOLANGCI_LINT_VERSION}" \
  > /etc/fleet-image-versions
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
INSTALL

echo "build: install toolchain in rootfs"
chroot "$ROOTFS" /bin/bash /tmp/install.sh
rm -f "$ROOTFS/tmp/install.sh"

# fc-supervisor as PID 1. No systemd/sshd/cron/dbus/cloud-init in a minbase rootfs.
install -m 0755 "$SUPERVISOR_BIN" "$ROOTFS/sbin/fc-supervisor"
ln -sf /sbin/fc-supervisor "$ROOTFS/sbin/init"

# Set env for module caches on the workspace volume.
cat >>"$ROOTFS/etc/environment" <<'ENVV'
GOMODCACHE=/workspace/.cache/go/mod
GOCACHE=/workspace/.cache/go/build
npm_config_cache=/workspace/.cache/npm
PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin:/sbin
ENVV

# Unmount before imaging so proc/dev/sys are not copied into the ext4.
for m in $mounted; do umount -l "$m" 2>/dev/null || true; done
mounted=""
rm -f "$ROOTFS/etc/resolv.conf"

echo "build: mke2fs (${SIZE_MB} MiB)"
mke2fs -q -t ext4 -d "$ROOTFS" -b 4096 "$WORK/rootfs.ext4" "${SIZE_MB}M"

SHA="$(sha256sum "$WORK/rootfs.ext4" | cut -d' ' -f1)"
zstd -q -19 -o "$OUT_DIR/rootfs-${SHA}.ext4.zst" "$WORK/rootfs.ext4"
printf '%s\n' "$SHA" >"$OUT_DIR/rootfs-${SHA}.sha256"
echo "build: wrote $OUT_DIR/rootfs-${SHA}.ext4.zst"
echo "$SHA"
