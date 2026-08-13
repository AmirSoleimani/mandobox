# Mandobox

**Agent runtime infrastructure you host yourself — a fresh, disposable computer for every task.**

Give an AI agent its own machine: an isolated, locked-down micro-VM, spun up for a single task and
thrown away when it's done. Mandobox is the runtime that makes that safe and repeatable — many
disposable computers running agents in parallel, on hardware you own, with keys that never leave it.
Point it at your codebase and every task comes back as a pull request you review; nothing merges
without you.

*Coding-to-PR is the flagship workload today. Underneath, the runtime is general — the agent harness
and the work it does are pluggable, not baked in.*

**Works with** &nbsp;
[![GitHub](https://img.shields.io/badge/GitHub-181717?logo=github&logoColor=white)](docs/github-setup.md)
[![Slack](https://img.shields.io/badge/Slack-4A154B?logo=slack&logoColor=white)](docs/slack.md)
[![Telegram](https://img.shields.io/badge/Telegram-26A5E4?logo=telegram&logoColor=white)](docs/telegram.md)
&nbsp;— review on GitHub, start & steer from Slack or Telegram.

---

## See it in action

> 🎥 **Demo coming soon.**
>
> <!-- Screen recording goes here. On GitHub you can drag-and-drop an .mp4/.mov straight into the
>      README editor (or any issue/PR) to get a hosted URL, then paste it below this line. -->

---

## What you can do

- **Delegate a task, get back a pull request** — "add a `/careers` page", "fix the login timeout",
  "bump this dependency and fix the fallout." The agent reads the repo, makes the change, runs the
  tests, and opens a PR.
- **Plan first, then build** — start a task with `--plan` and the agent explores your project and comes
  back with a plan instead of code. Talk it over in the thread — refine it, push back, ask questions —
  and it writes code only when you say go. It'll also stop and ask on its own if it hits a real fork in
  the road, rather than guessing.
- **It checks its own UI work** — for a change you can see in a browser, the agent renders the result in
  a real headless browser, looks at the screenshot, and fixes what's obviously broken *before* opening
  the PR. Ask to see it and it drops the screenshot straight into the chat thread.
- **Run many at once** — each in its own disposable computer, without interfering.
- **Review and steer on GitHub** — your PR comments and reviews drive the next round; the agent
  replies and revises right in the thread.
- **Or steer from chat** — kick off a task from Slack or Telegram, drop in a plan or spec file, and reply
  to nudge it along; when several tasks share one chat, reply to a specific one's message to steer just
  that task.
- **Watch it work live** — a built-in terminal streams its thinking, the commands it runs, and the
  files it edits, and you can **type back to steer it**.
- **Go hands-on** — open a running machine in full VS Code (in your browser) to poke around or edit
  by hand.

## How it works

1. **You ask** — from Slack, Telegram, or the dashboard, pointing at a repo and describing the change.
   Add `--plan` and it proposes an approach for you to refine and approve before it writes any code.
2. **It isolates** — a fresh, locked-down micro-VM boots just for this task.
3. **The agent works** — reads the repo, makes the change, runs the tests, and writes an honest
   summary, *including what it's unsure about.*
4. **You review** — it opens a pull request and answers your comments in the thread; you can drop
   into the running machine to look around or edit by hand.
5. **You decide** — merge, request changes, or close. Merging cleans everything up.

## Why it's built this way

- **You stay the reviewer.** Every result is a pull request. Nothing ships on its own.
- **Your hardware, your keys.** Code and credentials never leave a machine you control, and the AI
  provider key is never exposed to the agent's sandbox.
- **One throwaway computer per task.** Each job runs in an isolated micro-VM, destroyed after. Agents
  can't see each other and only reach the parts of the internet you allow.
- **It runs in parallel.** Many tasks side by side, without stepping on each other.
- **You talk to it where you already work** — Slack, Telegram, GitHub, or a browser control room.

## The control room

A built-in dashboard: one private screen (reached over an SSH tunnel — no public exposure) to start
tasks, watch an agent think and act in a live color-coded terminal, connect into a running machine,
and manage models, connectors, costs, health, and secrets from one place.

→ **[Dashboard guide](dashboard/README.md)**

## Configure it

- **Box-wide defaults** in one file (or the dashboard): the model, machine size, how long an idle
  machine stays warm, spend caps, and which agents/models are allowed.
- **Per-repo settings** in an optional `.mandobox.yml`, so a project can ask for a bigger machine or
  its own instructions — within the limits you set.
- **Pluggable agents & providers** — Claude Code by default (OpenAI Codex as a second harness); run
  on an API key or [your own Claude subscription](docs/subscription-auth.md).

→ **[Configuration guide](docs/configuration.md)** · [`mandobox.example.yml`](mandobox.example.yml) · [Visual preview](docs/preview.md)

## Roadmap

Anything that can describe a task can start one — today that's Slack, Telegram, GitHub, and the dashboard.
Next, we want work to reach the fleet from wherever it already lives, no new habits required
(**planned/exploring**, not shipped):

- **Connectors** — Linear / Jira / GitHub Issues (assign or label an issue → it opens the PR and
  moves the ticket along), Discord / Teams, Sentry (turn a recurring error into a fix task),
  scheduled chores, and a public API + webhooks.
- **Beyond** — more agent harnesses, team accounts with roles, per-repo budgets & alerts, and saved
  task templates.

Want a particular connector? [Open an issue](../../issues) — the roadmap follows what people reach for.

## Is this for you?

Mandobox is **self-hosted infrastructure**, not a one-click app: you run a server that supports
hardware virtualization, a GitHub App, and an AI provider key. It's built for **developers and teams
who want their own AI agent fleet on their own hardware.** Setup is scripted end-to-end, with a
friendly guide. If you want a hosted product with zero setup, this isn't that.

## Get started

- **[Getting started](docs/getting-started.md)** — the friendly, start-to-finish path.
- **[Server setup](docs/hetzner-setup.md)** · **[GitHub App](docs/github-setup.md)** ·
  **[Slack](docs/slack.md)** — the pieces it connects to.
- **[Operator runbook](docs/runbook.md)** — the detailed deploy guide and every config knob.
- **[How it's designed](docs/architecture.md)** — the full architecture: durable [Temporal](https://temporal.io/)
  workflows over [Firecracker](https://firecracker-microvm.github.io/) micro-VMs, and the reasoning
  behind it.

## License

[MIT](LICENSE). Contributions welcome.
