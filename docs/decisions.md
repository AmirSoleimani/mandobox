# Decisions

Deviations from [`PLAN.md`](PLAN.md), which remains the reference for component behaviour and
invariants. This file records where the build intentionally diverges.

## D5 — This is a permanent personal tool, not a product (2026-07-30)

PLAN §15 D5 asked whether this is a personal tool or a prototype of a product feature.
**Answer: a personal, single-operator tool.** No multi-tenancy, no RBAC, no billing, no
scaling for serving others (already out of scope in §1/§16 — this makes it permanent). It
optimises for simplicity of operation over productisation.

## Single-machine build

PLAN §2 describes two planes on two machines: a control plane (Hetzner Cloud CX) and the
fleet host (dedicated). **This build collapses them onto one box** — the dedicated KVM host
runs everything: fleet-agent, the egress gateway, NATS, the microVMs, and (at M4) Temporal,
webhook-rx, and the credential minter.

**Why it's safe.** The load-bearing trust boundary is guest (untrusted) ↔ host (trusted), and
that is enforced by nftables on the guest tap class — independent of where host services run.
A guest can still reach only the anchor's DNS (53), gateway (8080), and NATS (4222); it cannot
reach Temporal (7233), the fleet-agent API (9443), or anything else (all dropped and counted).
Invariants I1–I9 are unaffected.

**What we give up:** blast-radius separation between the control plane and the fleet host, and
independent reboot/patch of the two. Acceptable for a single-operator tool.

**What changed concretely:**
- NATS binds the host **anchor** (`172.31.0.1:4222`), not an external host. Guests reach it via
  an nftables **input** accept, not forward + masquerade. The guest→external NATS forward rule
  and the NAT/masquerade chain are removed; guests never forward anywhere.
- One inventory group (`fleet`); `deploy.yml` installs fleet-agent + reconciler + gateway +
  NATS. `control-plane.yml` and the `control` group are gone.
- M4's control-plane services co-locate on the same box.

## Earlier decisions (recorded elsewhere)

- **D1 — verification story:** guest runs linters + language unit tests only; full e2e is
  delegated to the target repo's GitHub CI. No Docker in the guest. (See `image/README.md`.)
- **Egress gateway** is a minimal Go service (key injection + allowlist + scrubbing), not
  LiteLLM; LiteLLM fronts it at M5 for model routing (PLAN §10).
