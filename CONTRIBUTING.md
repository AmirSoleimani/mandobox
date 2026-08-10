# Contributing to Mandobox

Thanks for your interest! Mandobox is self-hosted infrastructure for running AI coding agents in
isolated micro-VMs, so contributions range from Go services to Ansible roles to docs.

## Ground rules

- **Be honest in code and PRs.** No fake benchmarks, no "production-ready" claims without evidence.
  Say what's tested and what isn't.
- **Every change is reviewable.** Keep PRs focused and small enough to read.
- **Security matters here.** This project handles credentials and runs untrusted code in VMs. If a
  change touches the trust boundary (the egress gateway, credential handling, VM isolation, or the
  nftables rules), call it out explicitly in the PR.

## Getting set up

You can build and unit-test everything **without** a server:

```sh
make check      # go vet + all unit tests
make dist       # cross-compile the linux/amd64 binaries into bin/
```

Anything that launches a real VM (Firecracker/jailer/KVM) needs a Linux host with `/dev/kvm`; see
[`docs/runbook.md`](docs/runbook.md). The Go code isolates those bits behind interfaces, so most
logic is testable with fakes.

## Repo layout

- `cmd/` — the binaries (`mando-agent`, `mando-worker`, `mando-gateway`, `fc-supervisor`, …).
- `internal/` — the packages behind them (supervisor, control plane, gateway, VM manager).
- `ansible/` — provisioning and deployment roles.
- `image/` — the golden guest-image build.
- `docs/` — architecture, setup, and operator guides.

## Making a change

1. Fork and branch.
2. Keep `make check` green; add tests for new behavior.
3. Open a PR describing **what** changed and **why**, and note anything you couldn't verify.

## Reporting bugs and ideas

Open an issue with enough detail to reproduce. For anything security-sensitive, see
[`SECURITY.md`](SECURITY.md) instead of filing a public issue.
