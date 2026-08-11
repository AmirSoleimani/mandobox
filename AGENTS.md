# AGENTS.md

Guidance for AI coding agents (and humans) working in this repo. **mandobox** is agent-runtime
infrastructure: one disposable Firecracker micro-VM per task, driven by durable Temporal workflows, every
task ending in a GitHub PR. Full design → **[docs/architecture.md](docs/architecture.md)**. Contribution
basics → **[CONTRIBUTING.md](CONTRIBUTING.md)**. This file is the short list of what's non-obvious and
what will bite you.

## Build & test
- **`make check`** — `go vet` + tests for **both** Go modules: the root module and `dashboard/` (it has
  its own `go.mod`). Run it before finishing any change; it must be green.
- **`make dist`** — cross-compile the `linux/amd64` binaries into `bin/`.
- Anything that launches a real VM (Firecracker/jailer) needs a Linux host with `/dev/kvm`. The Go code
  isolates those bits behind interfaces, so most logic is unit-testable with fakes on any OS.

## Repo map
- `internal/control` — the Temporal control plane: `PRWorkflow` + its activities. **Policy lives here.**
- `internal/connectors` — chat connectors (Slack, Telegram). One `Connector` = inbound `Serve` + outbound
  `Notifier`; `cmd/mando-connectors` runs the enabled ones.
- `internal/fleetagent` — the Firecracker launcher (`mando-agent`). `internal/supervisor` — the guest PID 1
  (`fc-supervisor`). `internal/gateway` — the single egress proxy. Also `natsauth`, `reconcile`, `session`.
- `cmd/` — the binaries. `dashboard/` — the localhost management UI (separate module). `ansible/` —
  provisioning/deploy. `image/` — the golden guest-image build.

## Footguns — read before changing the control plane
- **Temporal determinism.** `PRWorkflow` is deterministic and replayed from history. Do NOT change the
  command stream (which activities/timers/signals run, and in what order) for in-flight workflows without a
  `workflow.GetVersion` gate — see existing gates (`meaningful-branch`, `no-pr-wait-for-input`, …).
  *Activity-side* changes (what an activity does internally) are safe. A structural workflow change must
  deploy on a **drained fleet** (0 running `PRWorkflow`s).
- **The `RegisterActivity` trap.** The worker registers the whole `*control.Activities` struct via
  `worker.RegisterActivity`, which reflects over **every exported method** and panics at boot on one that
  isn't a valid activity signature. **Never add an exported non-activity method to `Activities`.** (Chat
  connectors register their `Notifier` via the `Activities.Notifiers` map, not a method.) Guarded by
  `internal/control/register_test.go`.
- **Adding a chat connector** = one `Connector` in `internal/connectors` (implement `Serve` + `Notifier`) +
  a `Registry()` entry. Configuring/enabling is runtime — `connectors.json` (enable/disable) + the secret
  env — **no redeploy**, toggled from the dashboard. GitHub is the *substrate* (the PR), not a chat
  connector. See docs/architecture.md → "Adding a chat connector".
- **Message formatting** is canonical **Slack-mrkdwn**; each `Notifier` translates it (Slack passes it
  through; Telegram via `canonicalToTelegramHTML`). If you touch outbound messages, keep Slack output
  byte-identical — there are golden tests (`internal/control/render_test.go`).
- **Secrets are Tier-0.** The real provider key and the GitHub App key **never enter a guest VM** — the
  egress gateway injects the key host-side. Never add code that ships a Tier-0 secret into MMDS or a guest.

## Deploying
Provisioning and deploys are Ansible (`ansible/`): one dedicated `/dev/kvm` host, a systemd unit per
service. Two rules hold regardless of environment:
- **Never disrupt a live session.** Before rolling the worker or rebuilding the golden image, check for
  running work (`temporal workflow list … ExecutionStatus='Running'`) and live VMs. A session with an open
  PR is still live.
- **Determinism gates a worker swap.** Activity-side changes are safe any time; a structural `PRWorkflow`
  change must deploy on a drained fleet (see Footguns).

When overwriting a *running* binary in place (rather than via Ansible), write to a temp path and `mv` it —
a direct copy over a busy executable fails with `ETXTBSY`.

## House rules
- Match the surrounding code's style, comment density, and idioms.
- Keep public code and docs free of internal jargon / private identifiers (they've been sanitized — don't
  reintroduce them).
- Keep changes small and reviewable; `make check` green is the bar for "done".
