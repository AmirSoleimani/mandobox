# Operator runbook (M1–M3)

Linear guide from bare Hetzner boxes to a running fleet. Authoritative spec is
[`PLAN.md`](PLAN.md); provisioning detail is in [`hetzner-setup.md`](hetzner-setup.md).

## Machines

| Host | Type | Runs |
|---|---|---|
| **fleet** | Hetzner **dedicated** (AX/EX, `/dev/kvm`, root FS = **XFS**) | fleet-agent, reconciler, gateway, microVMs |
| **control** | Hetzner **Cloud CX** (small) | NATS (M3); Temporal etc. at M4 |

Full provisioning — including the **XFS root** choice (hard to change later) — is in
[`hetzner-setup.md`](hetzner-setup.md). Do that first.

## 0. Controller prerequisites (your workstation)

```sh
# Go 1.25+ (to cross-compile the binaries) and Ansible core 2.15+.
cd ansible && ansible-galaxy collection install -r requirements.yml && cd ..
```

## 1. Inventory

Edit `ansible/inventory/hosts.yml` — set `ansible_host` for `fleet-host-01` and `control-01`
(or copy it to `ansible/inventory/local.yml`, which is gitignored, and pass
`-i inventory/local.yml`). Accept the host keys once (`ssh root@<ip> true`).

## 2. Secrets

See [`../secrets/README.md`](../secrets/README.md).

```sh
scripts/gen-dev-certs.sh secrets/fleet-tls <fleet-host-ip>   # mTLS PKI (M2)
printf '%s' "sk-ant-..." > secrets/anthropic.key             # real key (M3 gateway, §9)
chmod 600 secrets/anthropic.key
```

## 3. Build the binaries

```sh
make check        # go vet + test (55 tests)
make dist         # -> bin/{fleet-agent,fleet-reconciler,fc-supervisor,fleet-gateway} (linux/amd64)
```

## 4. M1 — provision the fleet host

```sh
cd ansible
ansible-playbook site.yml          # run TWICE — the second run must report changed=0
ansible-playbook smoke-test.yml    # expect: "smoke: PASS — microVM reached userspace"
```

That is the M1 acceptance gate (§14): idempotent re-run + a throwaway microVM reaching
userspace.

## 5. M3 — control-plane NATS

```sh
ansible-playbook control-plane.yml   # installs nats-server on the control host
```

> M3 NATS is **no-auth dev mode**; keep the port private (only the fleet host reaches it, via
> nftables). M4 adds per-session JWT scoping.

## 6. M2 + M3 — deploy the fleet-host services

```sh
ansible-playbook deploy.yml          # fleet-agent + reconciler + gateway
```

Verify the API is up (mTLS):

```sh
curl --cert secrets/fleet-tls/reconciler.crt \
     --key  secrets/fleet-tls/reconciler.key \
     --cacert secrets/fleet-tls/server-ca.crt \
     https://<fleet-host-ip>:9443/healthz          # -> {"status":"ok"}
```

## 7. Golden image

The image needs a **Linux box with a rootfs builder** (`mke2fs`) — so not your Mac, and not
inside a guest µVM. The **fleet host itself qualifies**; you do not need a separate Docker
machine. Pick one:

**A. On the fleet host, Docker-free (recommended — one command, no extra box):**

```sh
make dist                            # controller: builds bin/fc-supervisor
cd ansible && ansible-playbook build-image.yml
```

This installs `mmdebstrap` on the fleet host, builds the rootfs, and writes
`rootfs-<sha>.ext4.zst` **directly into `/var/lib/fleet/images/`** (the playbook prints the
sha). Takes several minutes.

**B. CI (production path):** push the repo to GitHub and let
`.github/workflows/golden-image.yml` build it on Docker runners, then download the artifact
and `scp` it to `/var/lib/fleet/images/` on the fleet host.

**C. Any Linux box with Docker:** `bash image/build.sh` → `scp` the artifact to the fleet
host's `/var/lib/fleet/images/`.

Note the `<sha>` the build prints — you pass it as `IMAGE_SHA` in step 9.

## 8. GitHub App

Follow [`github-setup.md`](github-setup.md): create the App under the Chelodo org, install on
target repos, set branch protection + the `agent/*` ruleset. Needed for real PR runs.

## 9. Dispatch a task by hand (M3 end-to-end)

`scripts/dispatch-vm.sh` POSTs a launch to fleet-agent. It needs a **GitHub installation
token** — until M4's credential minter exists, mint one manually (App JWT → installation
token; `gh` or a short script). Then:

```sh
export FLEET_URL=https://<fleet-host-ip>:9443
export FLEET_TLS_CERT=secrets/fleet-tls/reconciler.crt
export FLEET_TLS_KEY=secrets/fleet-tls/reconciler.key
export FLEET_SERVER_CA=secrets/fleet-tls/server-ca.crt
export IMAGE_SHA=<sha from step 7>
export REPO_SLUG=chelodo/yourrepo
export REPO_CLONE_URL=https://github.com/chelodo/yourrepo.git
export GITHUB_TOKEN=<installation token>
export NATS_URL=nats://<control-ip>:4222
export PROMPT="Add a /healthz endpoint"
scripts/dispatch-vm.sh
```

Expected (M3 acceptance, §14): a PR opened by the **App bot** on an `agent/*` branch, **no
credential in the guest** (`env` dump + workspace grep), and a push to `main` from the App
**rejected** by GitHub.

## What is not wired yet (M4+)

Temporal orchestration, the webhook receiver, the NATS↔Temporal bridge, the credential minter
(automatic GitHub/gateway/NATS token issuance), Slack, and cost routing. Until then, dispatch
is manual (step 9) and NATS is unauthenticated.
