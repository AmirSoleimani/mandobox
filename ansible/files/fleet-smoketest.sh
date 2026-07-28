#!/usr/bin/env bash
# fleet-smoketest — M1 acceptance (PLAN §14): boot a throwaway Firecracker microVM on
# the installed Firecracker + guest kernel and confirm it reaches userspace.
#
# It builds a minimal busybox initramfs whose /init prints a unique marker to the serial
# console and immediately powers the VM off. No network, no root drive, no jailer — this
# proves only that KVM + Firecracker + the guest kernel boot to userspace. Exits 0 iff
# the marker appears on the console within the timeout.
set -euo pipefail

FC_BIN="${FLEET_FC_BIN:-/usr/local/bin/firecracker}"
KERNEL="${FLEET_KERNEL:-/var/lib/fleet/kernels/vmlinux}"
BOOT_TIMEOUT="${FLEET_SMOKE_TIMEOUT:-60}"
MARKER="FLEET-SMOKE-OK-$$-$(date +%s)"

fail() {
  echo "smoke: FAIL: $*" >&2
  exit 1
}

command -v "${FC_BIN}" >/dev/null 2>&1 || fail "firecracker not found at ${FC_BIN}"
[ -r "${KERNEL}" ] || fail "guest kernel not readable at ${KERNEL}"
[ -r /dev/kvm ] || fail "/dev/kvm not present — not a KVM-capable host"

BUSYBOX="$(command -v busybox || true)"
[ -x "${BUSYBOX}" ] || fail "busybox-static not installed"

WORK="$(mktemp -d)"
cleanup() { rm -rf "${WORK}"; }
trap cleanup EXIT

# --- Build the initramfs -------------------------------------------------------
ROOT="${WORK}/initramfs"
mkdir -p "${ROOT}/bin" "${ROOT}/dev" "${ROOT}/proc" "${ROOT}/sys"
cp "${BUSYBOX}" "${ROOT}/bin/busybox"
# The kernel wires stdio to /dev/console; /dev/null keeps busybox happy.
mknod -m 622 "${ROOT}/dev/console" c 5 1
mknod -m 666 "${ROOT}/dev/null" c 1 3

cat >"${ROOT}/init" <<INIT
#!/bin/busybox sh
/bin/busybox mount -t proc proc /proc 2>/dev/null
/bin/busybox mount -t sysfs sys /sys 2>/dev/null
/bin/busybox echo "${MARKER}"
/bin/busybox sync
# Trigger a reset so Firecracker exits (kernel cmdline sets reboot=k).
/bin/busybox reboot -f
INIT
chmod 0755 "${ROOT}/init"

( cd "${ROOT}" && find . -print0 | cpio --null -o --format=newc 2>/dev/null ) \
  | gzip -9 >"${WORK}/initramfs.cpio.gz"

# --- Firecracker config --------------------------------------------------------
cat >"${WORK}/vmconfig.json" <<CFG
{
  "boot-source": {
    "kernel_image_path": "${KERNEL}",
    "initrd_path": "${WORK}/initramfs.cpio.gz",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off rdinit=/init"
  },
  "machine-config": {
    "vcpu_count": 1,
    "mem_size_mib": 256
  }
}
CFG

# --- Boot ----------------------------------------------------------------------
echo "smoke: booting microVM (timeout ${BOOT_TIMEOUT}s, marker ${MARKER})"
CONSOLE="${WORK}/console.log"
timeout "${BOOT_TIMEOUT}" "${FC_BIN}" --no-api --config-file "${WORK}/vmconfig.json" \
  >"${CONSOLE}" 2>&1 || true

if grep -q "${MARKER}" "${CONSOLE}"; then
  echo "smoke: PASS — microVM reached userspace"
  exit 0
fi

echo "smoke: marker not found; console follows:" >&2
sed 's/^/smoke| /' "${CONSOLE}" >&2 || true
fail "microVM did not reach userspace within ${BOOT_TIMEOUT}s"
