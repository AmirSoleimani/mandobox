# Fleet host provisioning

Provisions a Hetzner **dedicated** box to launch Firecracker microVMs. Idempotent;
the authoritative architecture reference is [`../docs/architecture.md`](../docs/architecture.md).

## What it installs

| Role | Responsibility |
|---|---|
| _pre_tasks_ | Assert x86_64, `/dev/kvm`, and CPU virt flags; fail with a clear message |
| `base` | `fleet` user/group, directory layout, base packages, sysctls |
| `firecracker` | Firecracker v1.16.1 + jailer + guest kernel 6.1.155, checksum-verified |
| `networking` | IP forwarding, deny-by-default nftables, host DNS resolver, CNI plugins |
| `reaper` | Host reaper (systemd timer, 2 min) enforcing max VM lifetime |

## Usage

```sh
ansible-galaxy collection install -r requirements.yml
$EDITOR inventory/hosts.yml          # set the real host; or use an untracked local.yml
ansible-playbook site.yml
ansible-playbook smoke-test.yml       # acceptance smoke test: boots a throwaway microVM
```

## Off-host validation (no hardware needed)

```sh
ansible-lint --profile production
ansible-playbook site.yml --syntax-check
ansible-playbook smoke-test.yml --syntax-check
```

## Acceptance criteria

1. **Idempotent** — a second `site.yml` run reports `changed=0`.
2. **`ansible-lint` passes at production profile.**
3. **Smoke test** — `smoke-test.yml` boots a microVM on the installed Firecracker +
   kernel and observes it reach userspace (a marker printed on the serial console).

## Notes

- **Requires real hardware.** Hetzner Cloud has no `/dev/kvm`; preflight fails on it by
  design. Only the lint/syntax checks run off-host.
- **`fleet_nats_host`** in `group_vars/fleet.yml` is a placeholder (TEST-NET). Override
  it once the control-plane NATS host exists. The nftables policy renders a valid
  rule either way.
- Tap devices, per-VM tap IPs, and the per-VM state under `/run/fleet/vms/` are created
  at launch time by `mando-agent`. This playbook installs the policy and layout they depend on.
