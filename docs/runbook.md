# Operator runbook (M1–M3)

Linear guide from a bare Hetzner box to a running fleet. Authoritative spec is
[`PLAN.md`](PLAN.md); provisioning detail is in [`hetzner-setup.md`](hetzner-setup.md).

## Machine

This is a personal, **single-machine** build — one box runs everything. No separate control
plane.

| Host | Type | Runs |
|---|---|---|
| **fleet** | Hetzner **dedicated** (AX/EX, `/dev/kvm`, root FS = **XFS**) | fleet-agent, reconciler, gateway, NATS, microVMs |

Full provisioning — including the **XFS root** choice (hard to change later) — is in
[`hetzner-setup.md`](hetzner-setup.md). Do that first.

## 0. Controller prerequisites (your workstation)

```sh
# Go 1.25+ (to cross-compile the binaries) and Ansible core 2.15+.
cd ansible && ansible-galaxy collection install -r requirements.yml && cd ..
```

## 1. Inventory

Edit `ansible/inventory/hosts.yml` — set `ansible_host` for `fleet-host-01` (or copy it to
`ansible/inventory/local.yml`, which is gitignored, and pass `-i inventory/local.yml`).
Accept the host key once (`ssh root@<ip> true`).

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

## 5. Deploy the services (M2 + M3)

```sh
ansible-playbook deploy.yml          # fleet-agent + reconciler + gateway + NATS
```

> NATS is bound to the host anchor (`172.31.0.1:4222`) — reachable by guests via nftables,
> not on the public interface — and is **no-auth dev mode** for now. M4 adds per-session JWT
> scoping.

Verify the API is up (mTLS):

```sh
curl --cert secrets/fleet-tls/reconciler.crt \
     --key  secrets/fleet-tls/reconciler.key \
     --cacert secrets/fleet-tls/server-ca.crt \
     https://<fleet-host-ip>:9443/healthz          # -> {"status":"ok"}
```

## 6. Golden image

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

Note the `<sha>` the build prints — you pass it as `IMAGE_SHA` in step 8.

## 7. GitHub App

Follow [`github-setup.md`](github-setup.md): create the App under the Chelodo org, install on
target repos, set branch protection + the `agent/*` ruleset. Needed for real PR runs.

## 8. Dispatch a task by hand (M3 end-to-end)

`scripts/dispatch-vm.sh` POSTs a launch to fleet-agent. It needs a **GitHub installation
token**; mint a 1-hour one with `scripts/mint-github-token.sh` (App JWT → token — the manual
stand-in for M4's credential minter). `NATS_URL` defaults to the host anchor, so you can omit it.

```sh
export GITHUB_TOKEN="$(GITHUB_APP_ID=<app-id> \
  GITHUB_APP_KEY=secrets/<app>.private-key.pem \
  GITHUB_ORG=acme scripts/mint-github-token.sh)"

export FLEET_URL=https://<fleet-host-ip>:9443
export FLEET_TLS_CERT=secrets/fleet-tls/reconciler.crt
export FLEET_TLS_KEY=secrets/fleet-tls/reconciler.key
export FLEET_SERVER_CA=secrets/fleet-tls/server-ca.crt
export IMAGE_SHA=<sha from step 6>
export REPO_SLUG=acme/yourrepo
export REPO_CLONE_URL=https://github.com/acme/yourrepo.git
export PROMPT="Add a /healthz endpoint"
scripts/dispatch-vm.sh
```

Expected (M3 acceptance, §14): a PR opened by the **App bot** on an `agent/*` branch, **no
credential in the guest** (`env` dump + workspace grep), and a push to `main` from the App
**rejected** by GitHub.

## M4 — Temporal control plane (deployed)

Temporal now drives the full task lifecycle. Deploy with `ansible-playbook deploy.yml --tags
temporal,control_plane` (prerequisites on the controller: `make dist`; the GitHub App key at
`secrets/acmecloudagent.*.private-key.pem`; `openssl rand -hex 24 > secrets/webhook-secret`;
`openssl rand -hex 24 > secrets/temporal/postgres-password`).

**Services (all on the box, localhost-bound):**

| Service | Bind | Role |
|---|---|---|
| postgresql | 127.0.0.1:5432 | Temporal persistence + SQL visibility |
| temporal | 127.0.0.1:7233 (gRPC) | workflow engine, namespace `fleet` |
| temporal-ui | 127.0.0.1:8233 | Web UI — `ssh -L 8233:127.0.0.1:8233 root@<host>` |
| fleet-worker | — | hosts PRWorkflow + activities (task queue `fleet-pr`) |
| webhook-rx | 127.0.0.1:8088 | GitHub webhooks → Temporal signals (needs a public ingress/tunnel) |
| nats-bridge | — | archives guest event/log streams to `/var/lib/fleet/logs` |

**Dispatch a task (replaces the manual step 8):**

```sh
IMAGE_SHA=<sha> REPO_CLONE_URL=https://github.com/<owner>/<repo>.git REPO_SLUG=<owner>/<repo> \
  PROMPT="…" /usr/local/bin/fleet-dispatch     # run on the box
```

Watch it in the Temporal UI, or query: `temporal workflow query --namespace fleet
--workflow-id <session_id> --type status`. Resume/close a PR arrives via webhook-rx (or inject
a signal by hand: `temporal workflow signal --name review_comment --input '{…}'`).

**Verified end-to-end (all four M4 criteria):** initial phase → PR; resume-on-review-comment
(90s debounce) → a real second commit on the same branch (session continuity); delivery-ID
dedup; merge → workspace purged → workflow complete; no orphans left.

The headless agent runs with `--permission-mode bypassPermissions` and `IS_SANDBOX=1` (the
microVM is the sandbox, and Claude refuses bypass-as-root otherwise) so it can run bash tools;
it must NOT run git/gh itself — the supervisor commits, pushes, and opens/updates the PR. The
reaper additionally sweeps any Firecracker process left with no state dir (`ORPHAN_GRACE`).

## What is not wired yet (M5+)

Slack thread rendering + `needs_input` round-trip, the final cost summary, and LiteLLM model
routing (§10). NATS is still unauthenticated (single box); per-session JWT scoping is deferred.
