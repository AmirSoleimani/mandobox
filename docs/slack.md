# Slack integration

Slack is the fleet's human interface (the other is the Temporal UI). You dispatch tasks with a
slash command, watch each task's whole life in a dedicated thread, and steer a run by replying
in its thread — all without exposing anything to the public internet.

- **Outbound** (fleet → Slack): the Temporal worker posts a thread per session via `chat.postMessage`.
- **Inbound** (Slack → fleet): `slack-gateway` connects to Slack over **Socket Mode** (an outbound
  WebSocket), so Slack never needs to reach your box. It translates the `/fleet` command and thread
  replies into Temporal actions.

```
  You in Slack ──/fleet──▶ slack-gateway ──starts──▶ PRWorkflow (Temporal)
       ▲                        (Socket Mode)              │
       │                                                   │ activities
       └──────── thread posts ◀── fleet-worker ◀───────────┘
                 (chat.postMessage)      RunAgentPhase / PostSlack / …
```

Everything degrades gracefully: if the Slack secrets are absent, the worker's Slack posts become
no-ops and `slack-gateway` stays stopped — the fleet still runs, just without Slack.

---

## 1. Create the Slack app (one time, ~3 min)

1. Go to <https://api.slack.com/apps> → **Create New App** → **From scratch**. Name it (e.g. `fleet`)
   and pick your workspace.
2. **Socket Mode** (left nav) → toggle **Enable Socket Mode**. It generates an **App-Level Token**
   (`xapp-…`) with the `connections:write` scope — **copy it**.
3. **OAuth & Permissions** → **Bot Token Scopes**, add:
   | scope | why |
   |---|---|
   | `chat:write` | post the session thread |
   | `commands` | receive the `/fleet` slash command |
   | `app_mentions:read` | (optional) receive @-mentions |
   | `channels:history` | read thread replies in public channels |
   | `groups:history` | read thread replies in private channels |
4. **Slash Commands** → **Create New Command**: command `/fleet`, Request URL anything
   (`https://example.com` — Socket Mode ignores it), description "dispatch an agent task".
5. **Event Subscriptions** → **Enable Events** → **Subscribe to bot events**: `message.channels`
   (and `message.groups` for private channels), and `app_mention` if you added that scope.
6. **Install App** → **Install to Workspace** → copy the **Bot User OAuth Token** (`xoxb-…`).
7. Create or pick a channel (e.g. `#fleet`) and **invite the bot into it** (`/invite @fleet`). The bot
   only sees messages in channels it's a member of. Grab the channel ID: click the channel name →
   **View channel details** → the ID (`C0…`) is at the bottom.

> The bot token and app token are Tier-0 secrets — treat them like the GitHub App key.

---

## 2. Configure and deploy

On the controller (your Mac), save the three secrets (gitignored) and deploy:

```sh
printf '%s' 'xoxb-…' > secrets/slack-bot-token    # Bot User OAuth Token
printf '%s' 'xapp-…' > secrets/slack-app-token    # App-Level Token (Socket Mode)
printf '%s' 'C0…'    > secrets/slack-channel      # default channel id

cd ansible && ansible-playbook deploy.yml --tags control_plane
```

The `control_plane` role writes `/etc/fleet/slack.env` (mode 0600) on the box and starts
`slack-gateway`; the worker picks up `SLACK_BOT_TOKEN` / `SLACK_CHANNEL` and starts posting.

### Verify

```sh
ssh root@<box> 'systemctl is-active slack-gateway; journalctl -u slack-gateway -n 3 -o cat'
# → active
# → slack-gateway: connected as fleet (U0…)
```

Then in your channel: `/fleet chelodo/hello-gents add a CHANGELOG.md`. Within a few seconds a
thread appears.

### Configuration reference

| Setting | Where | Default | Meaning |
|---|---|---|---|
| `SLACK_BOT_TOKEN` | `secrets/slack-bot-token` | — | `xoxb-` token; empty ⇒ no Slack |
| `SLACK_APP_TOKEN` | `secrets/slack-app-token` | — | `xapp-` Socket Mode token |
| `SLACK_CHANNEL` | `secrets/slack-channel` | — | fallback channel for CLI dispatches |
| `CLAUDE_MODEL` | `slack-gateway` env | `claude-sonnet-5` | default model id |
| `CLAUDE_CHEAP_MODEL` | `slack-gateway` env | `claude-haiku-4-5-20251001` | model for `--cheap` |
| `BASE_BRANCH` | `slack-gateway` env | `main` | branch the agent forks from |

The image launched is always the currently active golden image (`/var/lib/fleet/images/current.sha`),
re-read on every dispatch — so rebuilding the image needs no gateway restart.

---

## 3. Using it

### Dispatch — `/fleet`

```
/fleet <owner/repo> <prompt>
/fleet --cheap <owner/repo> <prompt>
```

- `/fleet chelodo/hello-gents add a CONTRIBUTING guide` — dispatch on the default model (Sonnet).
- `/fleet --cheap chelodo/hello-gents fix the lint warnings` — dispatch on the cheap model (Haiku),
  for boring/mechanical work.

You get a private (ephemeral) acknowledgement, then the public thread opens in the same channel:

```
(only you see this)  Dispatched s_RX94… on chelodo/hello-gents — I'll open a thread here.
```

The thread is where the whole task unfolds. **One thread = one session = one PR.**

### Steer a run — reply in the thread

Reply **inside a session's thread** to talk to that run. What happens depends on timing:

- **While the agent is running** — your message is queued and handed to the agent at its next turn
  boundary (Claude Code is non-interactive, so it can't be interrupted mid-turn).
- **While the task is waiting for review** (no VM running) — your message becomes review feedback: it
  starts a new **review round** (batched over ~90 seconds, so several quick replies fold into one
  round) and the agent resumes on the same branch to address it.

This means you can run a whole review loop from Slack alone — dispatch, read the diff on GitHub, then
reply in the thread with changes.

### Ending a session

A session ends when the PR is merged or closed, the review budget is exhausted (5 rounds / $15 by
default), it's aborted, or it hits the 24-hour TTL. It then posts a final summary and discards the
workspace.

> Merge/close and GitHub review-comment triggers arrive via **GitHub webhooks**, which need a public
> endpoint for `webhook-rx` (not required for the Slack-only flow above — see
> [What needs the webhook receiver](#what-needs-the-webhook-receiver)). Without it, drive review
> rounds by replying in the thread, and end a session by merging on GitHub then aborting from the
> Temporal UI (or letting it TTL).

---

## 4. Scenarios

Thread mockups below show what actually gets posted (emoji shorthand: 🤖 dispatch · 🎉 PR opened ·
🔄 review round · ⬆️ push · ℹ️ no changes · ❔ needs input · 🔍 recovered · ❌ failed · 🏁 done).

### Scenario A — happy path, steered entirely from Slack

```
you:  /fleet chelodo/hello-gents add a LICENSE file (MIT, 2026, chelodo)

#fleet
┌─ 🤖 Task dispatched  s_RX94WFYM9GSV6J7N07SAP3B40T
│     repo chelodo/hello-gents   branch agent/s_RX94…
│     > add a LICENSE file (MIT, 2026, chelodo)
│  └─ 🎉 PR opened #7   (cost $0.2145 · 753 tokens)
│                                                    ← agent ran, opened the PR, VM torn down
│  you (reply in thread): also add a copyright header comment at the top
│  └─ 🔄 Review round 1 — addressing 1 item(s).      ← ~90s after your reply
│  └─ ⬆️ Pushed 7e61a22b   (cost $0.0965 · 495 tokens)
│                                                    ← you merge PR #7 on GitHub
│  └─ 🏁 Session complete — merged
│        PR #7 · rounds 1 · $0.3111 · 1248 tokens · 6m40s
└─
```

You did two things in Slack (the slash command and one reply); everything else is automatic.

### Scenario B — cheap model for mechanical work

```
you:  /fleet --cheap chelodo/hello-gents normalize all line endings to LF

┌─ 🤖 Task dispatched  s_8QP2…
│     repo chelodo/hello-gents   branch agent/s_8QP2…
│     > normalize all line endings to LF
│  └─ 🎉 PR opened #8   (cost $0.0121 · 402 tokens)   ← Haiku: ~15× cheaper than Sonnet here
└─
```

`--cheap` routes the run to Haiku via LiteLLM. Use it for lint fixes, formatting, CI triage — reserve
the default (Sonnet) for real implementation. The cost in the summary tells you whether the class was
worth it.

### Scenario C — steering an in-flight run

```
you:  /fleet chelodo/api add pagination to the /users endpoint

┌─ 🤖 Task dispatched  s_KD7…
│     > add pagination to the /users endpoint
│  you (reply while it's running): use cursor-based pagination, not offset
│                                                    ← queued; applied at the agent's next turn
│  └─ 🎉 PR opened #12  (cost $0.83 · 3204 tokens)
└─
```

Because Claude Code runs a turn to completion, your steer lands at the next turn boundary rather than
interrupting the current one. It still shapes the result.

### Scenario D — the agent needs input

```
┌─ 🤖 Task dispatched  s_M4A…
│     > migrate the config format
│  └─ ❔ Agent needs input: Two config schemas exist (v1, v2). Which should I migrate to?
│        Reply in this thread to continue.
│  you (reply): migrate to v2
│  └─ 🔄 Review round 1 — addressing 1 item(s).
│  └─ 🎉 PR opened #21  (cost $1.10 · 4001 tokens)
└─
```

Agents are told to make reasonable assumptions and finish, so this is rare — but when the agent truly
can't decide, it asks in the thread and your reply unblocks it.

### Scenario E — no changes, or a failure

```
┌─ 🤖 Task dispatched  s_ZZ1…
│     > remove the deprecated foo() helper
│  └─ ℹ️ Agent produced no changes this round.       ← nothing to remove; no PR
│  └─ 🏁 Session complete — no_pr
│        rounds 0 · $0.04 · 210 tokens · 41s
└─
```

```
┌─ 🤖 Task dispatched  s_ZZ2…
│  └─ ❌ Run failed at git_push: remote rejected (branch protection)
│  └─ 🏁 Session complete — no_pr · $0.06 · 55s
└─
```

A no-op run and a failure are both legitimate outcomes reported plainly — never silent.

### Scenario F — a recovered PR

```
│  └─ 🔍 Recovered PR #6 (its open event was lost in transit).
```

The agent talks to the workflow over NATS (at-most-once delivery). If the "PR opened" event is ever
lost, the workflow reconciles against GitHub and adopts the real PR instead of pretending nothing
happened — you'll see this line instead of 🎉.

---

## 5. Message reference

| Post | When | Format |
|---|---|---|
| 🤖 Task dispatched | session start (root) | session id, repo, branch, prompt |
| 🎉 PR opened | initial run opens a PR | `#N` link, cost, tokens |
| 🔍 Recovered PR | pr_opened event was lost | `#N` link |
| 🔄 Review round N | a resume round starts | count of items batched |
| ⬆️ Pushed `sha` | a resume round pushes commits | short sha, cost, tokens |
| ℹ️ no changes | a round produced no diff | — |
| ❔ Agent needs input | agent asks a question | the question |
| ❌ Run failed | a stage errored | stage + error |
| 🏁 Session complete | end of life | phase, PR link, rounds, $, tokens, wall time |

---

## 6. What needs the webhook receiver

Slack (Socket Mode) covers **dispatch + steering + review rounds** with no inbound network exposure.
Three triggers come from **GitHub** instead of Slack and need `webhook-rx` reachable from GitHub:

- a **review comment / "changes requested"** on the PR → auto review round,
- **merging or closing** the PR → workspace teardown + final summary,
- **CI status** (with `auto_fix_ci`, off by default).

`webhook-rx` listens on `127.0.0.1:8088`. To use GitHub triggers, expose it (e.g. a Cloudflare
tunnel, or a reverse proxy on the public interface — the HMAC secret in `secrets/webhook-secret`
authenticates deliveries) and point the GitHub App's webhook at it. Until then, use the Slack-only
flow: reply in the thread to drive review rounds, merge on GitHub, and let the session TTL (or abort
it from the Temporal UI).

---

## 7. Troubleshooting

| Symptom | Check |
|---|---|
| `/fleet` says "dispatch failed" | `journalctl -u slack-gateway`; is Temporal up? is the repo `owner/name`? |
| No thread appears after `/fleet` | worker Slack posts need `SLACK_BOT_TOKEN`; is the bot **in the channel**? re-run the `control_plane` deploy |
| Thread replies do nothing | bot needs `channels:history` + the `message.channels` event subscription, and must be a channel member; the reply must be **in the thread**, not top-level |
| `slack-gateway` won't start | it exits without both tokens — confirm `/etc/fleet/slack.env` has `SLACK_BOT_TOKEN` and `SLACK_APP_TOKEN` |
| Merges/review comments ignored | that's the GitHub webhook path — see §6 |
| Wrong/invalid model | dispatch a real model id (Claude Code expands its own aliases); LiteLLM passes it through |

---

## 8. Security notes

- **Socket Mode = no inbound exposure.** The bot dials out to Slack; your box accepts no Slack traffic.
- **Tokens are host-side.** Bot/app tokens live in `/etc/fleet/slack.env` (0600) and never enter a guest.
- **The gateway only translates.** It starts workflows and forwards `user_message` signals — all policy
  (debounce, dedupe, budgets, teardown) lives in the Temporal workflow, not in Slack.
- **Anyone who can post in the channel can dispatch.** Restrict the channel's membership accordingly;
  the fleet has no per-user authorization in v1.
