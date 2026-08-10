# Hetzner setup

This is a personal, **single-machine** build: you need **exactly one machine — the fleet
host** — which runs everything (mando-agent, gateway, NATS, microVMs, and later Temporal).
No separate control plane.

| Role | Machine | Needed for |
|---|---|---|
| **Fleet host** | Hetzner **Robot dedicated** (AX/EX), bare metal | everything |
| Source hosting | GitHub org (your GitHub organization) + GitHub App | PR workflow |

Why dedicated, not Cloud: Hetzner **Cloud has no `/dev/kvm`** (no nested virtualisation),
so Firecracker cannot run. The playbook's preflight refuses such a host on purpose.
Bare metal always exposes `vmx`/`svm` and Hetzner enables VT-x/AMD-V — no BIOS
change needed.

---

## 1. Order the dedicated server

Any current **AX (AMD)** or **EX (Intel)** model works. Size for your parallelism
target (e.g. ~5 concurrent VMs): **≥8 cores, 32–64 GB RAM, NVMe** is comfortable, since
each guest later gets its own vCPUs/RAM plus a workspace volume on disk. For the smoke
test alone, anything works.

## 2. Install the OS (Ubuntu 22.04/24.04 LTS or Debian 12)

The Ansible roles use `apt` and support **Ubuntu 22.04 (jammy) / 24.04 (noble) LTS** and
**Debian 12 (bookworm)**. All work identically; pick any.

1. Robot → **Rescue** → activate *Linux* rescue, note the root password.
2. Reboot into rescue, SSH in as `root`.
3. Run **`installimage`**, pick **Ubuntu 24.04** (or Debian 12), and edit the partition
   config (next section).
4. Install, then reboot into the new system.

Add your SSH public key so Ansible can log in as `root`: either attach a key to the
server in Robot, or paste it into the `installimage` config's `SSHKEYS` / drop it into
`authorized_keys` after install. Both base images ship `python3` (Ansible needs it on the
target).

> **Ubuntu note:** a few packages the playbooks need (`busybox-static`, `mmdebstrap`,
> `debian-archive-keyring`) live in the **universe** component, which Ubuntu Server enables
> by default. If you ever see `apt` can't find them, run `add-apt-repository universe`.

## 3. Filesystem — XFS recommended, ext4 fine

`mando-agent` copies the golden rootfs on every launch with `cp --reflink=auto`: an instant
copy-on-write clone on **XFS** (or Btrfs), or a **full copy on ext4**. So ext4 works — the
fallback just copies the ~2 GB rootfs per launch (a couple of seconds on SSD, longer on
spinning disks). **XFS is recommended** because it makes that copy instant, and you get to
pick it for free while reinstalling anyway.

Sample `installimage` config — set the root partition to `xfs` (leave everything else at
installimage's defaults for your drives; the `DRIVE`/`IMAGE` lines are prefilled):

```text
# ... DRIVE lines prefilled by installimage (e.g. /dev/sda /dev/sdb or /dev/nvme0n1 ...)
SWRAID 1
SWRAIDLEVEL 1
HOSTNAME fleet-host-01
BOOTLOADER grub

PART /boot ext4  1024M
PART swap  swap  32G
PART /     xfs   all
# IMAGE line is prefilled (Ubuntu-2404-noble-amd64-base.tar.gz, or Debian-12xx-...)
```

If you would rather keep a **separate data partition**, put **both** `/var/lib/fleet` and
the jailer chroot on it (same filesystem) and tell me — I'll repoint `fleet_jail_dir` in
`ansible/group_vars/fleet.yml` at it so the CoW source and destination stay on one FS.

Verify after install:

```sh
findmnt -no FSTYPE /            # xfs (or ext4 — both fine)
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

Those two runs are the acceptance gate: idempotent re-run, and a throwaway
microVM booting to userspace.

## 5. Firewall

No Hetzner hardware firewall is required — the on-host nftables ruleset governs guest
egress. The host's own inbound (SSH now, the `mando-agent` mTLS API later) is left open by
the ruleset; restrict SSH with `networking_mgmt_cidr`-style rules or the Robot firewall if
you want defence in depth. (See the nftables design note: deny-by-default
is enforced for the guest tap class, not for the host's own management ports.)

---

## Later milestones (not now)

- **Control-plane services** (Temporal, webhook-rx, the credential minter) run on
  **this same box**, not a separate machine. Guests still can't reach them (nftables confines
  the guest tap class to the anchor's DNS/gateway/NATS ports).
- **GitHub** — migrate target repos to your GitHub organization, create the GitHub App
  (`contents:write`, `pull_requests:write`, `checks:read`), and set branch protection.
