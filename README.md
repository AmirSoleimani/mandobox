# Agent VM Fleet

Isolated microVMs on owned hardware that run coding agents and terminate every unit of
work in a human-reviewed pull request. See [`docs/PLAN.md`](docs/PLAN.md) for the full
PRD, invariants, and build order — that document is authoritative; this README only
tracks implementation state.

## Planes

| Plane | Where | Trust |
|---|---|---|
| Control plane | Hetzner Cloud CX | trusted |
| Fleet host | Hetzner dedicated (Robot AX/EX, `/dev/kvm`) | trusted |
| Guest | Firecracker microVM, one per session | **untrusted** |

## Build order (PLAN §14)

| Milestone | Scope | Status |
|---|---|---|
| **M1** | Host provisioning (Ansible) | ✅ built — on-hardware acceptance pending |
| **M2** | `fleet-agent`: launch / destroy / reconcile | ✅ built — on-hardware acceptance pending |
| **M3** | Golden image + `fc-supervisor`, single shot | ✅ built — on-hardware acceptance pending |
| M4 | Temporal `PRWorkflow`, full lifecycle | ⏳ not started |
| M5 | Slack, cost, model routing | ⏳ not started |
| M6 | Parallelism | ⏳ not started |

Do not start a milestone until the previous one's acceptance criteria (PLAN §14) pass.

## Repository layout

```
docs/PLAN.md          Authoritative PRD and build order
docs/hetzner-setup.md What to provision in Hetzner before M1
docs/github-setup.md  GitHub App + branch protection (M3)
ansible/              host provisioning + deploy roles (see ansible/README.md)
image/                golden guest image build (Dockerfile + build.sh)  [M3]
cmd/                  fleet-agent, fleet-reconciler, fc-supervisor, fleet-gateway
internal/             session, fleetagent, reconcile, supervisor, gateway
scripts/              gen-dev-certs.sh (dev mTLS PKI)
.github/workflows/    golden-image.yml (CI image build)
Makefile              build / test / vet / dist (cross-compile)
```

Go services land under `cmd/` and `internal/` as their milestones begin. See
[`docs/hetzner-setup.md`](docs/hetzner-setup.md) for hardware prep.

## M2 — fleet-agent (launch / destroy / reconcile)

`fleet-agent` is the fleet host's mTLS microVM API (`POST/DELETE/GET /vms`, `/healthz`);
`fleet-reconciler` kills orphaned VMs the authority no longer claims (PLAN §7.1, §7.7).

```sh
make test            # 31 unit tests across session / fleetagent / reconcile
make dist            # cross-compile linux/amd64 binaries into bin/
scripts/gen-dev-certs.sh secrets/fleet-tls <fleet-host-ip>   # dev mTLS PKI
cd ansible && ansible-playbook deploy.yml                    # deploy to the fleet host
```

**Verified off-host:** unit tests, `go vet`, `gofmt`, and a live mTLS round-trip
(`/healthz` + `GET /vms` succeed only with a client cert; certless clients are rejected at
handshake). **Needs the box:** actual VM launch/destroy, orphan reaping within 12 min, and
workspace survival across a destroy — these exercise jailer/Firecracker/KVM, which the
driver isolates behind the `VMDriver` interface.

## M3 — golden image + fc-supervisor (single shot)

The guest runs `fc-supervisor` as PID 1 (`internal/supervisor`): reads boot config from
MMDS, refuses to run without NATS, runs Claude Code behind the host gateway, and opens a PR.
Full e2e verification is delegated to the target repo's **GitHub CI** (D1); the guest runs
only linters + language unit tests, so there is no Docker in the image.

- **Golden image** (`image/`) — content-addressed `rootfs-<sha>.ext4.zst`, built in CI
  (`.github/workflows/golden-image.yml`) on the pinned M1 kernel.
- **Egress gateway** (`internal/gateway`, `cmd/fleet-gateway`) — injects the real Anthropic
  key host-side (guest holds only a session token), enforces a CONNECT allowlist for
  git/registry traffic, scrubs the key from responses. One port (§10). LiteLLM slots in at M5.
- **NATS** (`ansible/roles/nats`) — minimal control-plane transport (M3 dev, no auth; JWT
  scoping in M4).
- **GitHub App** — see [`docs/github-setup.md`](docs/github-setup.md).

```sh
make dist                                       # builds all four binaries
printf '%s' "<real-anthropic-key>" > secrets/anthropic.key
bash image/build.sh                             # golden image (Linux + Docker; CI does this)
ansible-playbook control-plane.yml              # NATS on the control host
ansible-playbook deploy.yml                     # fleet-agent + reconciler + gateway
```

**Verified off-host:** the supervisor lifecycle (initial→PR, resume→push, agent-failure,
no-changes) and the gateway (key injection, session-token stripping, allowlist, scrubbing)
are unit-tested; the image build and Ansible roles are lint/syntax-clean. **Needs the box:**
a hand-dispatched task producing a real App-bot PR on `agent/*`, with no credential in the
guest, and a rejected push to `main`.

## M1 — Host provisioning

Provisions a Hetzner **dedicated** box to launch Firecracker microVMs: KVM preflight,
Firecracker + jailer, guest kernel, deny-by-default nftables, host resolver, directory
layout, and the host reaper timer.

```sh
cd ansible
ansible-galaxy collection install -r requirements.yml
# edit inventory/hosts.yml with your host, then:
ansible-playbook site.yml
ansible-playbook smoke-test.yml   # boots a throwaway microVM, asserts it reached userspace
```

### Pinned artifacts

| Artifact | Version | Source |
|---|---|---|
| Firecracker + jailer | v1.16.1 | GitHub releases (sha256-verified) |
| Guest kernel | 6.1.155 | Firecracker CI bucket (sha256-verified) |

Versions and checksums live in `ansible/group_vars/fleet.yml` and each role's
`defaults/main.yml`; bump them there.

> **This must run on real hardware.** Hetzner Cloud has no nested virtualisation, so
> there is no `/dev/kvm`; the playbook's preflight fails loudly on any host without it.
> `ansible-lint` and `--syntax-check` are the only checks that pass off-host.
