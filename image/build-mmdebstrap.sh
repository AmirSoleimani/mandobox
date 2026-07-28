#!/usr/bin/env bash
# build-mmdebstrap.sh — build the golden rootfs WITHOUT Docker, on any Debian/Ubuntu box
# (the fleet host itself works). Output is content-addressed rootfs-<sha>.ext4.zst.
#
# Requires: mmdebstrap, debian-archive-keyring, e2fsprogs (mke2fs), zstd, curl, and a
# prebuilt fc-supervisor binary (make dist -> bin/fc-supervisor). Run as root.
#
# Versions mirror image/Dockerfile — keep them in sync.
set -euo pipefail

SUPERVISOR_BIN="${SUPERVISOR_BIN:?set SUPERVISOR_BIN to the prebuilt fc-supervisor binary}"
OUT_DIR="${OUT_DIR:-/var/lib/fleet/images}"
SIZE_MB="${SIZE_MB:-2048}"
SUITE="${SUITE:-bookworm}"
MIRROR="${MIRROR:-http://deb.debian.org/debian}"

NODE_MAJOR="${NODE_MAJOR:-22}"
CLAUDE_CODE_VERSION="${CLAUDE_CODE_VERSION:-2.1.220}"
GH_VERSION="${GH_VERSION:-2.96.0}"
GOLANGCI_LINT_VERSION="${GOLANGCI_LINT_VERSION:-2.12.2}"
GO_VERSION="${GO_VERSION:-1.25.5}"

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

echo "build: mmdebstrap base ($SUITE)"
mmdebstrap --variant=minbase \
  --include=ca-certificates,curl,gnupg,git,jq,ripgrep,fd-find,python3,python3-venv,openssh-client,less,procps,iproute2,e2fsprogs \
  "$SUITE" "$ROOTFS" "$MIRROR"

# --- run installers inside the rootfs (needs network + pseudo-filesystems) ---
cp /etc/resolv.conf "$ROOTFS/etc/resolv.conf"
for fs in proc sys dev; do
  mount --bind "/$fs" "$ROOTFS/$fs"
  mounted="$ROOTFS/$fs $mounted"
done

cat >"$ROOTFS/tmp/install.sh" <<INSTALL
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
ln -sf "\$(command -v fdfind)" /usr/local/bin/fd

# Node LTS + pinned Claude Code CLI.
curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
apt-get install -y --no-install-recommends nodejs
npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"
npm cache clean --force

# GitHub CLI.
curl -fsSL -o /tmp/gh.tgz "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_amd64.tar.gz"
tar -xzf /tmp/gh.tgz -C /tmp
install -m 0755 "/tmp/gh_${GH_VERSION}_linux_amd64/bin/gh" /usr/local/bin/gh

# Go toolchain + golangci-lint.
curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
tar -C /usr/local -xzf /tmp/go.tgz
curl -fsSL -o /tmp/gcl.tgz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz"
tar -xzf /tmp/gcl.tgz -C /tmp
install -m 0755 "/tmp/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64/golangci-lint" /usr/local/bin/

# Python linter/formatter.
python3 -m venv /opt/pytools
/opt/pytools/bin/pip install --no-cache-dir ruff
ln -sf /opt/pytools/bin/ruff /usr/local/bin/ruff

# Provenance + cleanup.
printf 'claude-code=%s\nnode_major=%s\ngh=%s\ngo=%s\ngolangci-lint=%s\n' \
  "${CLAUDE_CODE_VERSION}" "${NODE_MAJOR}" "${GH_VERSION}" "${GO_VERSION}" "${GOLANGCI_LINT_VERSION}" \
  > /etc/fleet-image-versions
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
INSTALL

echo "build: install toolchain in rootfs"
chroot "$ROOTFS" /bin/bash /tmp/install.sh
rm -f "$ROOTFS/tmp/install.sh"

# fc-supervisor as PID 1 (§8.1). No systemd/sshd/cron/dbus/cloud-init in a minbase rootfs.
install -m 0755 "$SUPERVISOR_BIN" "$ROOTFS/sbin/fc-supervisor"
ln -sf /sbin/fc-supervisor "$ROOTFS/sbin/init"

# Set env for module caches on the workspace volume (§7.2).
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
