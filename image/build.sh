#!/usr/bin/env bash
# build.sh — build the content-addressed golden guest image.
#
# docker build -> docker export (rootfs tar) -> mke2fs -d (ext4, no privileged loop mount)
# -> zstd. The artifact is rootfs-<sha256-of-ext4>.ext4.zst; fleet-agent decompresses it to
# rootfs-<sha>.ext4 and reflink-copies it per launch.
#
# Requires (Linux CI): docker, mke2fs (e2fsprogs), zstd, sha256sum.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/dist/images}"
SIZE_MB="${SIZE_MB:-3072}" # ~2–3GB (headroom for the baked-in Chromium/Playwright)
IMAGE_TAG="fleet-golden:build"
EXPORT_NAME="fleet-golden-export-$$"

mkdir -p "$OUT_DIR"
WORK="$(mktemp -d)"
cleanup() {
  rm -rf "$WORK"
  docker rm -f "$EXPORT_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "build.sh: docker build"
docker build -f "$REPO_ROOT/image/Dockerfile" -t "$IMAGE_TAG" "$REPO_ROOT"

echo "build.sh: export rootfs"
docker create --name "$EXPORT_NAME" "$IMAGE_TAG" true >/dev/null
mkdir -p "$WORK/rootfs"
docker export "$EXPORT_NAME" | tar -x -C "$WORK/rootfs"
docker rm -f "$EXPORT_NAME" >/dev/null

echo "build.sh: build ext4 (${SIZE_MB} MiB)"
mke2fs -q -t ext4 -d "$WORK/rootfs" -b 4096 "$WORK/rootfs.ext4" "${SIZE_MB}M"

SHA="$(sha256sum "$WORK/rootfs.ext4" | cut -d' ' -f1)"
echo "build.sh: sha256=${SHA}"
zstd -q -19 -o "$OUT_DIR/rootfs-${SHA}.ext4.zst" "$WORK/rootfs.ext4"
printf '%s\n' "$SHA" >"$OUT_DIR/rootfs-${SHA}.sha256"
echo "build.sh: wrote $OUT_DIR/rootfs-${SHA}.ext4.zst"
echo "$SHA"
