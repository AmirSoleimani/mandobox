# mando-dashboard

The single-box management console for a mandobox fleet. It runs **on the fleet host** and is a
separate Go module (its own `go.mod`) — it talks to the box the same way an operator would: via the
Temporal SDK, by reading/writing the config file, and by shelling out to `update-tools.sh`. It does
not import `internal/control`; it decodes the workflow's `status` query into a small local struct
(`state.go`), so it stays decoupled from the control plane's internals.

## What it does (v1: observe + manage)

- **Sessions** — the recent `PRWorkflow` executions from Temporal (visibility list + live `status`
  query for running ones): status, repo, phase, branch, PR, VM state, review round, cost, start time.
  Auto-refreshes every 5s. **+ New session** dispatches a task; a **stuck** badge + **terminate**
  appears on wedged workflows; each row has a **watch/connect** button →
- **Health** — "is the box set up and running?": fleet services (systemctl), endpoint reachability
  (Temporal/LiteLLM/NATS/agent), active image, disk, VM count, secret presence — green/red.
- **Costs** — spend reconstructed from the archived per-session cost events (survives Temporal
  retention): total, by repo, by day, and the most expensive sessions.
- **Connect to Agent** — a terminal-like console per session (Sessions row → **connect**). The output
  is a live feed of what the agent is doing inside the VM: the guest streams Claude's `stream-json`
  over NATS, nats-bridge archives it to `<log-dir>/<session>.log.jsonl`, and the dashboard tails that
  file over **Server-Sent Events**, rendering each line readably (⚙ system · 💭 thinking · 🔧 tool call
  with args · 💬 text · ↳ result · 🏁 turn complete). The input is a **prompt line**: a message signals
  the workflow (`user_message`) exactly as Slack does — durable, coalesced into the next resume turn,
  relaunching the VM if it parked. So it's two-way for a Running session and read-only replay for a
  closed one. It's a turn-based conversation console (each turn ends in a PR update), not a raw VM
  shell — VS Code attach remains the hands-on "inside the VM" option.
- **VMs** — the live microVMs from fleet-agent's mTLS `GET /vms`: session, image, guest/host IP,
  vCPUs, mem, uptime, PID. Cross-references Temporal to flag **orphans** (a VM with no Running
  workflow — the reconciler will reap it). Observe-only; no launch/destroy from the UI.
- **Config** — view and edit `/etc/fleet/mandobox.yml` as raw YAML, with a parsed-key summary, a
  **key reference** (defaults + allowed values), and insert-snippet buttons. Saves are validated
  (must be a YAML mapping), written atomically, and keep a `.bak`. The worker re-reads the file per
  dispatch, so a change takes effect on the next task — no restart.
- **Instructions** — two things:
  - The box-wide default agent instructions (`/etc/fleet/agent-instructions.md`) — a plain-text
    system-prompt *addition* every session inherits unless its repo's `.mandobox.yml` sets its own.
  - The **base preambles** (the agent's built-in system prompts for autonomous vs collaborate turns).
    Each is editable to override per box, with a *reset to default* (the built-in text the worker
    materializes to `<path>.default`) and a warning on load-bearing lines. Empty override → built-in.
  Both are read by the worker per dispatch/launch, so edits are instant. (Requires the worker to
  support `MANDO_INSTRUCTIONS` / `MANDO_PREAMBLE_*` — a control-plane change.)
- **Tools** — the pinned agent-CLI versions (`tools.env`), the active image sha, and the update audit
  trail. Trigger an update (`update-tools.sh --claude … --codex …`): it assembles + activates a fresh
  golden image from the cached base (~40s) in the background while the page polls the live output.
  Running VMs keep their pinned image.
- **Secrets** — status + rotation for the box's secrets (Anthropic/OpenAI/LiteLLM keys, Slack tokens,
  webhook secret, GitHub App key, VS Code tunnel token). Shows presence, perms, mtime, and a short
  sha256 **fingerprint** — **never the value**. Rotating writes the new value (0600) and restarts the
  consuming service(s), resolving the real unit name on this box. Values are never returned or logged.
  Caveat surfaced in-UI: your Ansible `secrets/` on the controller stays the source of truth — a
  box-side rotation is reverted by the next `deploy.yml` unless you update the controller copy too.

## Access model

Binds to `127.0.0.1:8087` by default and carries **no authentication** — access is gated by who can
reach the box. Reach it over an SSH tunnel:

```
ssh -L 8087:127.0.0.1:8087 root@<box>
# then open http://localhost:8087
```

It runs as root on the box because it edits `/etc/fleet/mandobox.yml` and `update-tools.sh` chroots
and writes golden images. Do not bind it to a public interface.

## Build

```
cd dashboard
go build -o bin/mando-dashboard .          # host
GOOS=linux GOARCH=amd64 go build -o bin/mando-dashboard .   # for the box
go test ./...
```

The frontend (`web/`) is embedded via `go:embed`, so the binary is self-contained.

## Deploy (Ansible — recommended)

The `dashboard` role builds nothing itself; it copies the prebuilt binary, templates the env file and
systemd unit, and enables the service. From the repo root:

```
make dist-dashboard        # or `make dist` to build every binary
cd ansible
ansible-playbook -i inventory/local.yml deploy.yml --tags dashboard
```

The role installs `/usr/local/bin/mando-dashboard`, `/etc/fleet/dashboard.env`, and
`/etc/systemd/system/mando-dashboard.service`, then enables + starts it. Override any default
(`dashboard_addr`, `dashboard_temporal_address`, …) in `inventory/local.yml` or host_vars.

## Deploy (manual)

```
GOOS=linux GOARCH=amd64 go build -o bin/mando-dashboard .
scp bin/mando-dashboard root@<box>:/usr/local/bin/mando-dashboard
scp deploy/mando-dashboard.service root@<box>:/etc/systemd/system/
ssh root@<box> 'systemctl daemon-reload && systemctl enable --now mando-dashboard'
```

## Flags / env

| Flag | Env | Default |
|------|-----|---------|
| `-addr` | `MANDO_DASHBOARD_ADDR` | `127.0.0.1:8087` |
| `-temporal` | `TEMPORAL_ADDRESS` | `127.0.0.1:7233` |
| `-namespace` | `TEMPORAL_NAMESPACE` | `fleet` |
| `-config` | `MANDOBOX_CONFIG` | `/etc/fleet/mandobox.yml` |
| `-tools-env` | `MANDO_TOOLS_ENV` | `/usr/local/lib/fleet/tools.env` |
| `-update-tools` | `MANDO_UPDATE_TOOLS` | `/usr/local/lib/fleet/update-tools.sh` |
| `-audit` | `MANDO_TOOL_AUDIT` | `/var/lib/fleet/tool-updates.log` |
| `-images-dir` | `MANDO_IMAGES_DIR` | `/var/lib/fleet/images` |
| `-log-dir` | `FLEET_LOG_DIR` | `/var/lib/fleet/logs` |
| `-fleet-url` | `FLEET_URL` | `https://127.0.0.1:9443` |
| `-tls-cert` | `FLEET_TLS_CERT` | `/etc/fleet/tls/reconciler.crt` |
| `-tls-key` | `FLEET_TLS_KEY` | `/etc/fleet/tls/reconciler.key` |
| `-tls-ca` | `FLEET_SERVER_CA` | `/etc/fleet/tls/server-ca.crt` |
| `-secret-audit` | `MANDO_SECRET_AUDIT` | `/var/lib/fleet/secret-rotations.log` |
| `-instructions` | `MANDO_INSTRUCTIONS` | `/etc/fleet/agent-instructions.md` |
| `-preamble-autonomous` | `MANDO_PREAMBLE_AUTONOMOUS` | `/etc/fleet/preamble-autonomous.md` |
| `-preamble-collaborate` | `MANDO_PREAMBLE_COLLABORATE` | `/etc/fleet/preamble-collaborate.md` |

## API (JSON)

- `GET /api/sessions` → `{ sessions: [...] }`
- `GET /api/sessions/{id}/activity` → Server-Sent Events; parsed agent-activity feed (scrollback + tail)
- `POST /api/sessions/{id}/message` (`{ text }`) → signals `user_message` to the session's workflow
- `POST /api/sessions/{id}/abort` / `.../attach` / `.../terminate` → session controls
- `POST /api/dispatch` (`{ repo, prompt, base_branch?, model?, keep_alive? }`) → start a new session
- `GET /api/health` → grouped health checks · `GET /api/costs` → aggregated spend
- `GET /api/vms` → `{ vms: [...] }` (fleet-agent /vms, orphan-flagged)
- `GET /api/config` / `PUT /api/config` (`{ raw }`)
- `GET /api/config/schema` → key reference + snippets + annotated example
- `GET /api/instructions` / `PUT /api/instructions` (`{ raw }`) → box-wide default agent instructions
- `GET /api/preambles` / `PUT /api/preambles` (`{ name, raw }`) → base-preamble overrides (+ defaults)
- `GET /api/tools` → versions + audit + running job
- `POST /api/tools/update` (`{ claude, codex }`) → starts a background update job
- `GET /api/secrets` → status/fingerprint/perms/mtime per secret (never values)
- `POST /api/secrets/rotate` (`{ name, value }`) → writes the value 0600 + restarts consumers

## Not in v1 (deferred)

Dispatching/attaching sessions from the UI, per-VM destroy, revealing secret values, cost charts over
time, multi-user auth, and websockets. The design keeps to *observe + manage* (plus scoped rotation)
so the surface stays small and safe on a single box.
