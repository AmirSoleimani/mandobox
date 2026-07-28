# secrets/

Local secret material the deploy playbooks ship to the hosts. **Everything here except this
README is gitignored — never commit real keys.** These are Tier-0 (§9): they live on your
controller and the trusted hosts, never in a guest.

Populate before running `ansible-playbook deploy.yml`:

## `fleet-tls/` — mTLS PKI for fleet-agent ↔ reconciler (M2)

```sh
scripts/gen-dev-certs.sh secrets/fleet-tls <fleet-host-ip>
```

Produces `server.{crt,key}`, `client-ca.crt` (fleet-agent) and `reconciler.{crt,key}`,
`server-ca.crt` (reconciler). Dev PKI — replace with your real CA for production.

## `anthropic.key` — the real Anthropic API key (M3 gateway)

```sh
printf '%s' "sk-ant-..." > secrets/anthropic.key
chmod 600 secrets/anthropic.key
```

The egress gateway injects this host-side so the guest never holds it (I1).

## GitHub App private key (M4)

The App's private key (see `docs/github-setup.md`) is also Tier-0. Store it here (e.g.
`secrets/github-app.pem`) when M4's credential minter needs it. Not used by M1–M3 deploys.
