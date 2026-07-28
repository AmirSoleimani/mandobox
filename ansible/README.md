# M1 — Fleet host provisioning

Provisions a Hetzner **dedicated** box to launch Firecracker microVMs. Idempotent;
authoritative spec is [`../docs/PLAN.md`](../docs/PLAN.md) §7 and §14 (M1).

## What it installs

| Role | Responsibility | PLAN |
|---|---|---|
| _pre_tasks_ | Assert x86_64, `/dev/kvm`, and CPU virt flags; fail with a clear message | §7 |
| `base` | `fleet` user/group, directory layout, base packages, sysctls | §7.2, I7 |
| `firecracker` | Firecracker v1.16.1 + jailer + guest kernel 6.1.155, checksum-verified | §7.1, §7.2 |
| `networking` | IP forwarding, deny-by-default nftables, host DNS resolver, CNI plugins | §7.4, I2/I3 |
| `reaper` | Host reaper (systemd timer, 2 min) enforcing max VM lifetime | §7.6, I8 |

## Usage

```sh
ansible-galaxy collection install -r requirements.yml
$EDITOR inventory/hosts.yml          # set the real host; or use an untracked local.yml
ansible-playbook site.yml
ansible-playbook smoke-test.yml       # M1 acceptance: boots a throwaway microVM
```

## Off-host validation (no hardware needed)

```sh
ansible-lint --profile production
ansible-playbook site.yml --syntax-check
ansible-playbook smoke-test.yml --syntax-check
```

## M1 acceptance criteria (PLAN §14)

1. **Idempotent** — a second `site.yml` run reports `changed=0`.
2. **`ansible-lint` passes at production profile.**
3. **Smoke test** — `smoke-test.yml` boots a microVM on the installed Firecracker +
   kernel and observes it reach userspace (a marker printed on the serial console).

## Notes

- **Requires real hardware.** Hetzner Cloud has no `/dev/kvm`; preflight fails on it by
  design. Only the lint/syntax checks run off-host.
- **`fleet_nats_host`** in `group_vars/fleet.yml` is a placeholder (TEST-NET). Override
  it once the control-plane NATS host exists (M4). The nftables policy renders a valid
  rule either way.
- Tap devices, per-VM tap IPs, and the per-VM state under `/run/fleet/vms/` are created
  at launch time by `fleet-agent` (M2). M1 installs the policy and layout they depend on.
