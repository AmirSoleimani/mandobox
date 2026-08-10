# secrets/

Local secret material the deploy playbooks ship to the hosts. **Everything here except this
README is gitignored — never commit real keys.** These are Tier-0: they live on your
controller and the trusted hosts, never in a guest.

Populate before running `ansible-playbook deploy.yml`:

## `fleet-tls/` — mTLS PKI for mando-agent ↔ reconciler

```sh
scripts/gen-dev-certs.sh secrets/fleet-tls <fleet-host-ip>
```

Produces `server.{crt,key}`, `client-ca.crt` (mando-agent) and `reconciler.{crt,key}`,
`server-ca.crt` (reconciler). Dev PKI — replace with your real CA for production.

## `anthropic.key` — the real Anthropic API key

```sh
printf '%s' "sk-ant-..." > secrets/anthropic.key
chmod 600 secrets/anthropic.key
```

The egress gateway injects this host-side so the guest never holds it.

## GitHub App private key

The App's private key (see `docs/github-setup.md`) is also Tier-0. Store it here (e.g.
`secrets/github-app.pem`) when the credential minter needs it. Not used by earlier deploys.
