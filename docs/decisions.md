# Decisions

Notable design decisions and deviations from the original plan. [`architecture.md`](architecture.md)
remains the reference for component behaviour and invariants; this file records where the build
intentionally diverges.

## This is a permanent personal tool, not a product (2026-07-30)

The question was whether this is a personal tool or a prototype of a product feature.
**Answer: a personal, single-operator tool.** No multi-tenancy, no RBAC, no billing, no
scaling for serving others (already out of scope — this makes it permanent). It
optimises for simplicity of operation over productisation.

## Single-machine build

The original plan describes two planes on two machines: a control plane (Hetzner Cloud CX) and the
fleet host (dedicated). **This build collapses them onto one box** — the dedicated KVM host
runs everything: mando-agent, the egress gateway, NATS, the microVMs, Temporal,
webhook-rx, and the credential minter.

**Why it's safe.** The load-bearing trust boundary is guest (untrusted) ↔ host (trusted), and
that is enforced by nftables on the guest tap class — independent of where host services run.
A guest can still reach only the anchor's DNS (53), gateway (8080), and NATS (4222); it cannot
reach Temporal (7233), the mando-agent API (9443), or anything else (all dropped and counted).
The trust-boundary invariants are unaffected.

**What we give up:** blast-radius separation between the control plane and the fleet host, and
independent reboot/patch of the two. Acceptable for a single-operator tool.

**What changed concretely:**
- NATS binds the host **anchor** (`172.31.0.1:4222`), not an external host. Guests reach it via
  an nftables **input** accept, not forward + masquerade. The guest→external NATS forward rule
  and the NAT/masquerade chain are removed; guests never forward anywhere.
- One inventory group (`fleet`); `deploy.yml` installs mando-agent + reconciler + gateway +
  NATS. `control-plane.yml` and the `control` group are gone.
- The control-plane services co-locate on the same box.

## Filesystem — XFS recommended, ext4 supported

Rootfs copies use `cp --reflink=auto`: an instant copy-on-write clone on XFS/Btrfs, and a
full copy on ext4. **XFS root is still recommended** (instant, no per-launch copy cost), but
ext4 works — the fallback copies the ~2 GB rootfs per launch (seconds on SSD, longer on HDD),
which is fine at personal scale. Because everything lives under one root filesystem, the
workspace/kernel hardlinks into the jailer chroot are same-FS and work on either.

## Hard TTL stays PR-lifetime (14d), guarded by workflow versioning (2026-08-03)

The original plan recommended a **24h** hard TTL while iterating, to avoid the tax of versioning
workflows parked for days across frequent redeploys. We chose the opposite trade: the session must
live **as long as the PR is open** (a reviewer's back-and-forth can span days), so `HardTTL` is
14d — the abandoned-PR backstop, not a working limit. The redeploy tax that warning was about is instead
paid with `workflow.GetVersion` gates on structural workflow changes (see the runbook's
"Redeploying the control plane safely"). This keeps long-lived PR conversations intact without
wedging them on a worker redeploy.

## Earlier decisions (recorded elsewhere)

- **Verification story:** guest runs linters + language unit tests only; full e2e is
  delegated to the target repo's GitHub CI. No Docker in the guest. (See `image/README.md`.)
- **Egress gateway** is a minimal Go service (key injection + allowlist + scrubbing), not
  LiteLLM; LiteLLM fronts it for model routing.
