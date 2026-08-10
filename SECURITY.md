# Security Policy

Mandobox runs AI coding agents — which execute untrusted code — while holding real credentials, so
its security model is central, not incidental.

## Reporting a vulnerability

**Please do not open a public issue for security problems.** Instead, report privately via GitHub's
["Report a vulnerability"](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
button on this repository (Security → Advisories), or email the maintainers.

Please include what you did, what you expected, and what happened. We'll acknowledge and work with
you on a fix and disclosure timeline.

## The security model (what we protect, and how)

- **The guest is untrusted.** Each task runs in its own Firecracker micro-VM. Isolation is enforced
  by the hypervisor (KVM) and by nftables: the forward path is deny-by-default, so a guest cannot
  reach another guest, and its only outbound path is a host-side proxy.
- **Tier-0 credentials never enter a guest.** The real AI provider key lives on the host; the guest
  holds only a per-session token that the host-side gateway exchanges. The GitHub token a guest
  holds is a short-lived, scoped installation token that can only push to `agent/*` branches — never
  to `main` (which is protected on GitHub).
- **Egress is controlled.** All guest network traffic is forced through the gateway, which runs in
  `strict` mode (allowlist only) or `open` mode (any host, still logged). See the "Egress policy"
  section of [`docs/runbook.md`](docs/runbook.md).
- **Every change is human-reviewed.** Nothing an agent produces is merged automatically — the pull
  request is the gate.

## Your responsibilities as an operator

- Never commit real secrets. The `.gitignore` excludes `secrets/`, keys, and local inventories —
  keep it that way.
- Keep the host patched and its inbound surface minimal (the control-plane services bind to
  localhost; only what you expose is reachable).
- Rotate credentials if you suspect exposure, and prefer `strict` egress mode when running code you
  don't fully trust.

## Scope

This project is provided under the [MIT License](LICENSE) with no warranty. You run it on your own
infrastructure and are responsible for its operation and the code your agents run.
