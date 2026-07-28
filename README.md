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
| M3 | Golden image + `fc-supervisor`, single shot | ⏳ not started |
| M4 | Temporal `PRWorkflow`, full lifecycle | ⏳ not started |
| M5 | Slack, cost, model routing | ⏳ not started |
| M6 | Parallelism | ⏳ not started |

Do not start a milestone until the previous one's acceptance criteria (PLAN §14) pass.

## Repository layout

```
docs/PLAN.md          Authoritative PRD and build order
docs/hetzner-setup.md What to provision in Hetzner before M1
ansible/              M1 host provisioning + M2 deploy role (see ansible/README.md)
cmd/                  fleet-agent, fleet-reconciler (M2)
internal/             session, fleetagent, reconcile
scripts/              gen-dev-certs.sh (dev mTLS PKI)
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
