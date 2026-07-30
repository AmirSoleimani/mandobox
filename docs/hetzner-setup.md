# Hetzner setup

This is a personal, **single-machine** build: you need **exactly one machine — the fleet
host** — which runs everything (fleet-agent, gateway, NATS, microVMs, and later Temporal).
No separate control plane.

| Role | Machine | Needed for |
|---|---|---|
| **Fleet host** | Hetzner **Robot dedicated** (AX/EX), bare metal | everything |
| Source hosting | GitHub org (Chelodo) + GitHub App | M3/M4 |

Why dedicated, not Cloud: Hetzner **Cloud has no `/dev/kvm`** (no nested virtualisation),
so Firecracker cannot run. The playbook's preflight refuses such a host on purpose
(PLAN §7). Bare metal always exposes `vmx`/`svm` and Hetzner enables VT-x/AMD-V — no BIOS
change needed.

---

## 1. Order the dedicated server

Any current **AX (AMD)** or **EX (Intel)** model works for M1. Size for your parallelism
target (PLAN M6 = 5 concurrent VMs): **≥8 cores, 32–64 GB RAM, NVMe** is comfortable, since
each guest later gets its own vCPUs/RAM plus a workspace volume on disk. For the M1 smoke
test alone, anything works.

## 2. Install Debian 12 (bookworm)

The Ansible roles use `apt` and target Debian bookworm.

1. Robot → **Rescue** → activate *Linux* rescue, note the root password.
2. Reboot into rescue, SSH in as `root`.
3. Run **`installimage`**, pick **Debian 12**, and edit the partition config (next section).
4. Install, then reboot into the new system.

Add your SSH public key so Ansible can log in as `root`: either attach a key to the
server in Robot, or paste it into the `installimage` config's `SSHKEYS` / drop it into
`authorized_keys` after install. The Hetzner Debian base image already ships `python3`
(Ansible needs it on the target); if a stripped image ever lacks it, `apt-get install -y
python3` before running the playbook.

## 3. ⚠️ Filesystem — the one choice that's painful to change later

M2's `fleet-agent` **reflink-copies** the golden rootfs on every launch (PLAN §7.1).
Reflink is copy-on-write and requires an **XFS (reflink on by default) or Btrfs**
filesystem — **ext4 cannot reflink**, and reflink also fails *across* two filesystems.

**Simplest safe choice: make the root filesystem XFS.** Then the image cache
(`/var/lib/fleet/images`) and the per-VM copies under the jailer chroot (`/srv/jailer`)
sit on one reflink-capable filesystem automatically.

Sample `installimage` config for a 2×NVMe box with RAID1 — the only change from the
default is `xfs` on `/`:

```text
DRIVE1 /dev/nvme0n1
DRIVE2 /dev/nvme1n1
SWRAID 1
SWRAIDLEVEL 1
HOSTNAME fleet-host-01
BOOTLOADER grub

PART /boot ext4  1024M
PART swap  swap  32G
PART /     xfs   all

# IMAGE line is filled in by installimage (Debian-12xx-bookworm-amd64-base.tar.gz)
```

If you would rather keep a **separate data partition**, put **both** `/var/lib/fleet` and
the jailer chroot on it (same filesystem) and tell me — I'll repoint `fleet_jail_dir` in
`ansible/group_vars/fleet.yml` at that partition so the reflink source and destination
stay on one filesystem. Otherwise reflink silently falls back to a full copy or errors.

Verify after install:

```sh
xfs_info / | grep -q 'reflink=1' && echo "reflink OK"
ls -l /dev/kvm && echo "kvm OK"
```

## 4. Point Ansible at the host

On your workstation (not the server):

```sh
cd ansible
ansible-galaxy collection install -r requirements.yml
$EDITOR inventory/hosts.yml        # set ansible_host to the server IP, ansible_user: root
ssh root@<ip> true                 # accept the host key once (host_key_checking is on)
ansible-playbook site.yml          # run twice → expect changed=0 the second time (idempotent)
ansible-playbook smoke-test.yml    # expect: "smoke: PASS — microVM reached userspace"
```

Those two runs are the M1 acceptance gate (PLAN §14): idempotent re-run, and a throwaway
microVM booting to userspace.

## 5. Firewall

No Hetzner hardware firewall is required — the on-host nftables ruleset governs guest
egress. The host's own inbound (SSH now, the `fleet-agent` mTLS API in M2) is left open by
the ruleset; restrict SSH with `networking_mgmt_cidr`-style rules or the Robot firewall if
you want defence in depth. (See the nftables design note in the M1 summary: deny-by-default
is enforced for the guest tap class, not for the host's own management ports.)

---

## Later milestones (not now)

- **Control-plane services** (Temporal, webhook-rx, the credential minter — **M4**) run on
  **this same box**, not a separate machine. Guests still can't reach them (nftables confines
  the guest tap class to the anchor's DNS/gateway/NATS ports).
- **GitHub** — migrate target repos to the **Chelodo org**, create the GitHub App
  (`contents:write`, `pull_requests:write`, `checks:read`), and set branch protection
  (**M3/M4**, PLAN §11).
