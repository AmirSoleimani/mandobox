# Linear connector

Label a Linear issue **`mando`** and the agent picks it up, opens a PR, comments the link back on the
issue, and moves the issue through your workflow — **In Progress → In Review → Done/Canceled**. Comment on
the issue to steer the run, exactly like replying in a chat thread.

The repo the PR lands in is **inferred from the issue's title and description by a cheap LLM** — name it in
the issue as `owner/repo`. The **GitHub App's installed repos are the boundary**: the agent can only ever
touch repos the App can reach, and one it can't reach fails visibly on the issue. If the LLM can't tell which
repo, it asks instead of guessing.

## What you get

- **Pickup** — adding the `mando` label to a to-do issue dispatches a task (title + description = the prompt)
  and moves the issue to *In Progress*.
- **PR link** — when the PR opens, the agent comments the link on the issue and moves it to *In Review*.
- **Steering** — a comment on the issue is delivered to the run as a message (the same natural-language
  steering you get from Slack/Telegram/PR comments).
- **Screenshots** — when the task changes the UI, the agent uploads before/after screenshots as comments on
  the issue (the same visual self-verification it posts in chat).
- **Close-out** — merging the PR moves the issue to *Done*; closing it without merging moves it to *Canceled*.

## Setup

1. **API key — use a dedicated bot user, not your personal account.** The connector authenticates *as*
   whoever owns this key and ignores comments from its own identity (to avoid an echo loop). A personal key
   makes the bot *you* — your own replies get dropped as "its own" and every bot comment posts under your
   name. Invite a new member (a `you+mando@example.com` alias is fine), name it *mando*, add it to the team,
   and from **that** account create the key: Settings → Security & access → Personal API keys → *New key*.
   Paste it into the dashboard secret **Linear API key** (or `secrets/linear-api-key` for an Ansible deploy).
   The worker uses it to comment + move states; the connector uses it to read issues and identify itself.
2. **The `mando` label.** Create a label named `mando` in your workspace/team. Adding it to an issue is what
   hands the work over.
3. **Webhook.** Linear → Settings → API → Webhooks → *New webhook*:
   - **URL:** `https://<your-host>/linear`
   - **Events:** Issues + Comments
   - Copy the **signing secret** into the dashboard secret **Linear webhook signing secret** (or
     `secrets/linear-webhook-secret`).
4. **Public ingress.** The connector serves the webhook on `LINEAR_WEBHOOK_ADDR` (default `0.0.0.0:8089`).
   Linear requires HTTPS, so open that port at your cloud firewall and front it with your TLS reverse proxy.
   One proxy can front both receivers: `/webhook` → `webhook-rx` (:8088), `/linear` → this connector (:8089).
5. **Repo scope (optional).** By default the repo is inferred by the LLM and bounded only by which repos your
   GitHub App is installed on — no config needed. To *restrict* the agent to a specific set, set
   `LINEAR_REPO_ALLOWLIST` to the `owner/name` repos it may work on (space/comma-separated); the LLM then must
   pick one of them and the answer is validated against the list. Optionally set `LINEAR_DEFAULT_REPO` for a
   fixed fallback when a repo can't be inferred.
6. **Enable** Linear in the dashboard **Connectors** page, then add the `mando` label to a to-do issue.

> Add the label while the issue is still in a **to-do** column (Triage/Backlog/Todo). The connector only
> picks up unstarted issues, so it doesn't re-grab something already In Progress or Done.

## Exposing the webhook (HTTPS)

Linear requires HTTPS, but the connector itself just serves plain HTTP on `:8089` and verifies an HMAC — so
there's no lock-in to any front-end. The repo ships two optional, mutually-exclusive Ansible roles for the
public URL: a **Cloudflare Tunnel** (`--tags cloudflared` — no inbound port, hides the origin IP, domain on
Cloudflare) or **Caddy** (`--tags caddy` — self-hosted, any DNS, Let's Encrypt; you open 80 + 443). The same
ingress also fronts GitHub's `/webhook`, so the setup steps for both live in the runbook:
**[Webhook ingress](runbook.md#webhook-ingress-https)**.

Point the Linear webhook at `https://<host>/linear`. With a tunnel the connector needs no public port, so you
can set `LINEAR_WEBHOOK_ADDR=127.0.0.1:8089` (localhost-only).

**If you front it with Cloudflare**, two gotchas (Caddy has neither — its `reverse_proxy` passes the body
through untouched):
- **Don't let Cloudflare transform the request body** — the HMAC is over the *raw* POST bytes, so any
  rewrite → `401`. A plain tunnel passes bodies through; just don't enable body-altering features.
- **Bot/WAF rules can block Linear's server-to-server POSTs** — if deliveries 401/403, add a WAF skip rule
  for `/linear` (or allowlist Linear's webhook egress IPs).

Verify: `curl https://<host>/linear` returns **401** — the connector rejecting an unsigned request = the
path reaches it.

## Repo inference

The prompt is the issue title + description. To choose the repo:

- **No allowlist** (default) → a cheap model reads the issue and names the repo as `owner/repo` (the issue is
  expected to name it explicitly). The GitHub App's installed repos are the boundary — one it can't reach
  fails visibly on the issue (`:x: Run failed…`). If it can't name a well-formed `owner/repo` → a clarifying
  comment and **no dispatch**.
- **1 repo in the allowlist** → it's used (no LLM call).
- **Several in the allowlist** → the model picks the single best match *from the list* (recent human comments
  are included as context) and the answer is validated against it; uncertain → the default repo if set,
  otherwise it asks. Never dispatches to a repo outside the list.

Either way, reply on the issue with the repo and it re-tries. Dispatching to the wrong codebase is the
outcome to avoid, so the agent asks rather than guess a malformed slug.

## Security model

Adding the `mando` label is full trigger authority: **anyone who can label an issue can start an agent
run**, and the issue's title/description become the agent's prompt. Two consequences to size for:

- **The GitHub App installation is the blast radius.** A run can only ever touch repos the App is installed
  on, so *that installation is your control surface* — install the App only on repos you're comfortable any
  labeler running the agent against. (Setting `LINEAR_REPO_ALLOWLIST` narrows it further, to a subset.)
- **Workspace membership = who can dispatch.** Treat "can label issues in this workspace" as "can run the
  agent." Deliveries are HMAC-verified so only Linear can reach the endpoint, but authorization *within*
  Linear is your workspace's membership.

Everything still lands as a reviewable PR (nothing merges on its own), and the API key + webhook secret are
host-side only — never injected into a guest VM.

## Lifecycle → Linear state mapping

| Moment | Stage | Target state |
|---|---|---|
| Picked up | `in_progress` | a *started* state (prefers one named "In Progress") |
| PR opened | `in_review` | a *started* state (prefers "In Review"/"Review") |
| PR merged | `done` | a *completed* state (prefers "Done") |
| PR closed | `canceled` | a *canceled* state |

Mapping is by Linear's stable state **types** with a name preference, so it works across custom per-team
boards. If a team has no matching state, the move is simply skipped (never an error).

## Environment / secrets

| Var | Where | Purpose |
|---|---|---|
| `LINEAR_API_KEY` | `linear.env` (0600) | worker + connector; personal API key |
| `LINEAR_WEBHOOK_SECRET` | `linear.env` | connector; HMAC secret to verify deliveries |
| `LINEAR_WEBHOOK_ADDR` | `linear.env` | connector listen address (default `0.0.0.0:8089`) |
| `LINEAR_REPO_ALLOWLIST` | `linear.env` | *optional* — restrict inference to these `owner/name` repos |
| `LINEAR_DEFAULT_REPO` | `linear.env` | *optional* — fixed fallback repo when inference is unclear |

The API key + webhook secret are Tier-0: host-side only, never injected into a guest VM.

## Troubleshooting

- **401 on deliveries** — the signing secret doesn't match, or a proxy is altering the request body (the
  HMAC is over the raw bytes). Check the secret and that the proxy passes the body through unmodified.
- **Issue not picked up** — check it has the `mando` label, is in a *to-do* state, and Linear is enabled in
  Connectors (and if you set an allowlist, that the repo is in it). `journalctl -u mando-connectors` shows the decision.
- **It keeps asking which repo** — the LLM couldn't name a well-formed `owner/repo` from the issue; put an
  explicit `owner/repo` in the title/first line, or set `LINEAR_DEFAULT_REPO`.
- **Comments don't steer** — most often the API key belongs to your *personal* account, so the connector
  shares your identity and drops your comments as its own (the fix: a dedicated bot user's key — see setup).
  Separately, the connector must resolve its own user id at startup; if it can't (bad key) it disables
  steering (fail-closed) and logs it. Dispatch still works either way.
- **Repo inference follows the active provider** — on a subscription box it calls Anthropic directly on the
  OAuth token; on an API-key provider it goes through the gateway. Both use the provider's cheap model
  (`claude-haiku-4-5-20251001` by default). If no provider is configured at all it can't infer and asks on
  every issue; set `LINEAR_DEFAULT_REPO` or a single-repo allowlist to skip the LLM.
