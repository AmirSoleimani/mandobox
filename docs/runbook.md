# Operator runbook

Linear guide from a bare Hetzner box to a running fleet, and the reference for every configuration
knob. New here? Start with [`getting-started.md`](getting-started.md). Architecture and rationale
are in [`architecture.md`](architecture.md); provisioning detail is in [`hetzner-setup.md`](hetzner-setup.md).

> Steps 0–8 are the deploy order; the sections after cover the control plane, Slack, and
> running tasks in parallel.

## Machine

This is a personal, **single-machine** build — one box runs everything. No separate control
plane.

| Host | Type | Runs |
|---|---|---|
| **fleet** | Hetzner **dedicated** (AX/EX, `/dev/kvm`, root FS = **XFS**) | mando-agent, worker (PRWorkflow + scheduled reconcile), gateway, NATS, microVMs |

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
scripts/gen-dev-certs.sh secrets/fleet-tls <fleet-host-ip>   # mTLS PKI
printf '%s' "sk-ant-..." > secrets/anthropic.key             # real key (the gateway)
chmod 600 secrets/anthropic.key
```

## 3. Build the binaries

```sh
make check        # go vet + test (55 tests)
make dist         # -> bin/{mando-agent,fc-supervisor,mando-gateway,mando-worker,…} (linux/amd64)
```

## 4. Provision the host

```sh
cd ansible
ansible-playbook site.yml          # run TWICE — the second run must report changed=0
ansible-playbook smoke-test.yml    # expect: "smoke: PASS — microVM reached userspace"
```

This is the acceptance gate: idempotent re-run + a throwaway microVM reaching
userspace.

## 5. Deploy the services

```sh
ansible-playbook deploy.yml          # mando-agent + gateway + NATS + control plane (worker)
```

> NATS is bound to the host anchor (`172.31.0.1:4222`) — reachable by guests via nftables,
> not on the public interface — and is **no-auth dev mode** for now. per-session JWT scoping is a possible future hardening.
>

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

The on-host build is **split** (docs/configuration.md decision 2b): a heavy, cached **base**
(`mmdebstrap` + node + gh + vscode-cli + go + golangci + ruff) built once, and a fast **assemble**
(the pinned agent CLIs + `fc-supervisor`). `build-image.yml` builds the base on first run, then
assembles — so re-provisioning after a code change only re-assembles.

## Updating agent CLIs (Claude Code, Codex)

The agent CLIs ship updates often; the OS base rarely changes. Because the build is split, bumping a
CLI rebuilds only the small `assemble` step (~1 minute), not the whole base. Versions live in one
manifest — `image/tools.env` (`CLAUDE_CODE_VERSION`, `CODEX_VERSION`) — the single source of truth.

**Update a CLI** (on the fleet host):

```sh
/usr/local/lib/fleet/update-tools.sh --codex 0.x.y     # or --claude 2.1.221, or both
```

It pins the version(s), assembles a new image from the cached base, **verifies the CLI actually runs**
(the build fails if it doesn't — pin + verify), activates it, and appends to
`/var/lib/fleet/tool-updates.log` (the audit trail). **Running VMs keep their pinned image**; only new
dispatches use the new one — same safety as a full rebuild, in ~a minute.

**Rebuild the base** (rare — only when node/gh/go/golangci/vscode-cli versions in `image/mkimage.sh`
change): `/usr/local/lib/fleet/mkimage.sh base`, then run `update-tools.sh`.

`update-tools.sh` is the single audited entry point for "upgrade a tool to a version" — the operation
the planned management dashboard will drive.

## 7. GitHub App

Follow [`github-setup.md`](github-setup.md): create the App under your GitHub org, install on
target repos, set branch protection + the `agent/*` ruleset. Needed for real PR runs.

## 8. Dispatch a task by hand

`scripts/dispatch-vm.sh` POSTs a launch to mando-agent. It needs a **GitHub installation
token**; mint a 1-hour one with `scripts/mint-github-token.sh` (App JWT → token — the manual
stand-in for the credential minter). `NATS_URL` defaults to the host anchor, so you can omit it.

```sh
export GITHUB_TOKEN="$(GITHUB_APP_ID=<app-id> \
  GITHUB_APP_KEY=secrets/<app>.private-key.pem \
  GITHUB_ORG=your-org scripts/mint-github-token.sh)"

export FLEET_URL=https://<fleet-host-ip>:9443
export FLEET_TLS_CERT=secrets/fleet-tls/reconciler.crt
export FLEET_TLS_KEY=secrets/fleet-tls/reconciler.key
export FLEET_SERVER_CA=secrets/fleet-tls/server-ca.crt
export IMAGE_SHA=<sha from step 6>
export REPO_SLUG=your-org/yourrepo
export REPO_CLONE_URL=https://github.com/your-org/yourrepo.git
export PROMPT="Add a /healthz endpoint"
scripts/dispatch-vm.sh
```

Expected: a PR opened by the **App bot** on an `agent/*` branch, **no
credential in the guest** (`env` dump + workspace grep), and a push to `main` from the App
**rejected** by GitHub.

## Control plane (Temporal workflows)

Temporal now drives the full task lifecycle. Deploy with `ansible-playbook deploy.yml --tags
temporal,control_plane` (prerequisites on the controller: `make dist`; the GitHub App key at
`secrets/<your-app>.private-key.pem`; `openssl rand -hex 24 > secrets/webhook-secret`;
`openssl rand -hex 24 > secrets/temporal/postgres-password`).

**Services (all on the box, localhost-bound):**

| Service | Bind | Role |
|---|---|---|
| postgresql | 127.0.0.1:5432 | Temporal persistence + SQL visibility |
| temporal | 127.0.0.1:7233 (gRPC) | workflow engine, namespace `fleet` |
| temporal-ui | 127.0.0.1:8233 | Web UI — `ssh -L 8233:127.0.0.1:8233 root@<host>` |
| mando-worker | — | hosts PRWorkflow + activities (task queue `fleet-pr`) |
| webhook-rx | 127.0.0.1:8088 | GitHub webhooks → Temporal signals (needs a public ingress/tunnel) |
| nats-bridge | — | archives guest event/log streams to `/var/lib/fleet/logs` |

**Dispatch a task:** open the dashboard (`ssh -L 8087:127.0.0.1:8087 root@<host>` → `http://localhost:8087`)
and hit **+ New session** — repo, base branch, prompt, and **Keep-alive**. Or, if Slack is set up,
`/mando <owner>/<repo> <prompt>` in a channel.

**Keep-alive** controls how long a warm VM idles before it parks: a duration (`30m`, `2h`) or **`never`**
to keep it warm for the PR's whole life (bounded by `HardTTL`, and it holds a `MaxVMs` slot while up —
use it for a session you're actively working in or want to attach to, not by default). Empty → the 15m
default. Model and resources come from the resolved config (box + per-repo `.mandobox.yml`).

Watch it in the Temporal UI, or query: `temporal workflow query --namespace fleet
--workflow-id <session_id> --type status`. Resume/close a PR arrives via webhook-rx (or inject
a signal by hand: `temporal workflow signal --name review_comment --input '{…}'`).

**Verified end-to-end:** initial phase → PR; resume-on-review-comment
(coalesced into one turn) → a real second commit on the same branch (session continuity); delivery-ID
dedup; merge → workspace purged → workflow complete; no orphans left.

The headless agent runs with `--permission-mode bypassPermissions` and `IS_SANDBOX=1` (the
microVM is the sandbox, and Claude refuses bypass-as-root otherwise) so it can run bash tools;
it must NOT run git/gh itself — the supervisor commits, pushes, and opens/updates the PR. The
reaper additionally sweeps any Firecracker process left with no state dir (`ORPHAN_GRACE`).

## Remote attach (VS Code in a live VM)

Sometimes you want to step inside a running session's VM yourself — look around, or make an edit by
hand. Attach opens a browser **VS Code** (via `code tunnel`) onto the live microVM, scoped to that
one session.

**Using it (everyday):** in the session's Slack thread, just say so in your own words — "let me jump
in", "I want to edit this myself". The agent starts a tunnel and replies with a `https://vscode.dev/…`
link; open it and you're in the working tree. When you're done, say so ("I'm out", "you can take it
back") — it stops the tunnel and asks what to do with anything you changed (commit, discard, or hand
back to the agent). Attach keeps the VM warm while you're in it; dispatch the session with
`KEEP_ALIVE=never` if you want it to stay up regardless of idle time.

**One-time setup (optional but recommended):** without it, the first attach to each VM prints a
`github.com/login/device` code you complete once per VM. To skip that entirely, provision a
pre-authenticated tunnel token so every VM is already logged in:

```sh
# on the box, once:
code tunnel user login                       # complete the GitHub device code it prints
cp ~/.vscode/cli/token.json secrets/vscode-tunnel-token.json   # save it as a controller secret
# then redeploy the control plane (or just: systemctl restart mando-worker)
```

The worker injects that token — plus the host's own hostname — into every VM, and the guest adopts
the hostname so the token validates (the VS Code CLI binds its stored login to the hostname). The
token is scoped to your tunnels, not a broad credential; `secrets/` is gitignored. If you minted it
on a different host, set `VSCODE_TUNNEL_HOSTNAME` in the worker env to that host's name.

## Slack & model routing

**LiteLLM (deployed):** the egress gateway now proxies the LLM path to a native LiteLLM proxy
(`127.0.0.1:4000`, `litellm` role) which holds the real Anthropic key and routes by the `model`
field. Config is a wildcard passthrough (`* → anthropic/*`) since Claude Code sends real model
IDs; add `litellm_model_overrides` to send a model to another provider. Dispatch with a real ID:
default `claude-sonnet-5`, cheap `claude-haiku-4-5-20251001` (`CLAUDE_MODEL=…` or `/mando --cheap`).
The gateway injects the LiteLLM master key under `x-litellm-api-key`.

**Slack (code-complete; needs your tokens):** one thread per session with milestones + a final
cost summary; `/mando [--cheap] <owner/repo> <prompt>` dispatches; a thread reply steers the run
(user_message). Socket Mode — no public ingress. Full guide with scenarios: [`slack.md`](slack.md).
To activate:

```sh
printf '%s' 'xoxb-…' > secrets/slack-bot-token     # bot token
printf '%s' 'xapp-…' > secrets/slack-app-token     # app-level token (Socket Mode)
printf '%s' 'C0…'    > secrets/slack-channel       # default channel id
ansible-playbook deploy.yml --tags control_plane
```

Without those, the worker's Slack posts no-op and `mando-connectors` skips the Slack connector —
everything else runs unchanged.

## Egress policy (what the agent can reach)

Every guest's network is forced through `mando-gateway` — nftables drops all other egress (forward
policy `drop`, no NAT), so the gateway is the single chokepoint for git clones, package installs,
and any outbound traffic. Two knobs control it: the **mode** and the **allowlist**.

### Modes
- **`strict`** (default) — default-deny; only allowlisted hosts are reachable (CONNECT). Best when
  running repos you don't fully trust.
- **`open`** — any host reachable, but every CONNECT is still logged. Zero registry maintenance;
  you keep exfil *detection* (the audit log) and the kill-switch, but not exfil *prevention*.
  Reasonable for your own trusted repos.

Both modes keep all traffic flowing through the gateway, so the audit trail is complete either way:

```sh
journalctl -u mando-gateway -f | grep CONNECT   # host + mode for every egress; "denied" for blocks
```

### Change the mode — persistent (Ansible is the source of truth)

```yaml
# inventory/host_vars/<host>.yml   (or group_vars)
egress_gateway_mode: open          # or: strict
```
```sh
ansible-playbook -i inventory/hosts.yml deploy.yml --tags egress_gateway
```

### Change it quickly on the box — temporary (Ansible reverts it next run)

```sh
sed -i -E 's/-mode (strict|open)/-mode open/' /etc/systemd/system/mando-gateway.service
systemctl daemon-reload && systemctl restart mando-gateway
journalctl -u mando-gateway --since '5s ago' | grep '"mode"'   # confirm
```

The binary resolves the mode as `-mode` flag → `FLEET_GW_MODE` env → default `strict`.

### Add hosts to the allowlist (strict mode)

The built-in list already covers the mainstream registries (git, npm/yarn/pnpm, PyPI, crates.io,
RubyGems, Maven, NuGet, Composer, Go modules, and the Debian/Ubuntu apt mirrors). To reach a
private or niche registry, add hosts — they **extend** the built-in list, they don't replace it:

```yaml
# inventory/host_vars/<host>.yml
egress_gateway_allowlist:
  - registry.mycompany.internal
  - .my-cdn.example.com          # a leading "." matches the domain and all its subdomains
```
```sh
ansible-playbook -i inventory/hosts.yml deploy.yml --tags egress_gateway
```

A blocked host is easy to spot: the agent's PR notes it in Verification/Risks ("couldn't reach
`<host>`"), and the gateway logs it as `CONNECT denied` — so you see exactly what to add. To change
the baked-in set for *all* deployments, edit `DefaultAllowlist` in `internal/gateway/gateway.go`.

## Redeploying the control plane safely

A `PRWorkflow` lives as long as its PR (up to the 14-day backstop), so a worker redeploy can land
mid-flight. Temporal replays a running workflow's history against the **new** code; if the new
code produces a different *sequence* of commands (activities/timers/signals) than history recorded,
the workflow task fails with a non-determinism error and the PR wedges. (Changing an activity's
*input* — e.g. reply text — is safe; only the command *structure* matters.)

Rules for changing `internal/control/workflow.go`:

- **Structural change** (add/remove/reorder an activity or timer, change control flow around one):
  gate it with `workflow.GetVersion(ctx, "<change-id>", workflow.DefaultVersion, N)` and keep the
  `DefaultVersion` branch behaving exactly as the old code. In-flight workflows replay the old
  path; new workflows get the new one. The thread-reconcile feature (`"pr-thread-reconcile"`) is
  the worked example.
- **Input-only change** (message wording, prompt text): no gate needed.
- **Binary swap**: `scp` to a temp path then `mv` onto `/usr/local/bin/*` (atomic; a direct
  overwrite of a running binary hits `ETXTBSY`), then `systemctl restart mando-worker`.

If you must ship an ungated structural change, terminate the affected in-flight workflows first
(`temporal workflow terminate …`) — acceptable for throwaway test PRs, never for a real one.

## Running tasks in parallel

Concurrency was sound by design and is now verified end-to-end: the mando-agent serialises
allocation under a mutex, so N concurrent launches each get a unique tap + non-overlapping `/30`
(session-derived names, no collisions); `MaxVMs` (default 8) caps concurrency; nftables gives
isolation.

**Verified:** five sessions dispatched at once opened five distinct PRs
with no interference; a probe run *inside* a guest could not reach any sibling guest (`tap→tap`
packets dropped — the drop counter incremented, zero succeeded) nor the open internet (gateway
403), while the allowlisted registry still worked; each workflow reported its own distinct cost
(no cross-session leakage). Raise throughput by bumping `MaxVMs` (12 cores / 62 GB comfortably
runs more than 8× 2vCPU/4 GB VMs).

NATS is still unauthenticated (single box); per-session JWT scoping is deferred. Per-repo
egress policy (vs. the box-wide `egress_gateway_mode`) is a possible follow-up.
