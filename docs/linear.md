# Linear connector

Label a Linear issue **`mando`** and the agent picks it up, opens a PR, comments the link back on the
issue, and moves the issue through your workflow — **In Progress → In Review → Done/Canceled**. Comment on
the issue to steer the run, exactly like replying in a chat thread.

The repo the PR lands in is **inferred from the issue's title and description by a cheap LLM**, grounded to
an allowlist of the repos you let the agent touch. If it can't tell, it asks on the issue instead of guessing.

## What you get

- **Pickup** — adding the `mando` label to a to-do issue dispatches a task (title + description = the prompt)
  and moves the issue to *In Progress*.
- **PR link** — when the PR opens, the agent comments the link on the issue and moves it to *In Review*.
- **Steering** — a comment on the issue is delivered to the run as a message (the same natural-language
  steering you get from Slack/Telegram/PR comments).
- **Close-out** — merging the PR moves the issue to *Done*; closing it without merging moves it to *Canceled*.

## Setup

1. **API key.** Linear → Settings → Security & access → Personal API keys → *New key*. Paste it into the
   dashboard secret **Linear API key** (or `secrets/linear-api-key` for an Ansible deploy). The worker uses
   it to comment + move states; the connector uses it to read issues and verify itself.
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
5. **Repo allowlist.** Set `LINEAR_REPO_ALLOWLIST` to the `owner/name` repos the agent may work on
   (space/comma-separated). This both grounds the LLM's repo inference and validates its answer — the agent
   never dispatches to a repo outside this list. Optionally set `LINEAR_DEFAULT_REPO` for when a repo can't
   be inferred (otherwise it asks on the issue).
6. **Enable** Linear in the dashboard **Connectors** page, then add the `mando` label to a to-do issue.

> Add the label while the issue is still in a **to-do** column (Triage/Backlog/Todo). The connector only
> picks up unstarted issues, so it doesn't re-grab something already In Progress or Done.

## Repo inference

The prompt is the issue title + description. To choose the repo:

- **1 repo in the allowlist** → it's used (no LLM call).
- **Several** → a cheap model picks the single best match *from the allowlist* (recent human comments are
  included as context); the answer is validated against the list. Anything uncertain → the default repo if
  set, otherwise a clarifying comment on the issue and **no dispatch**. Reply on the issue and it re-tries.

Fail-safe: dispatching to the wrong codebase is the dangerous outcome, so the agent asks rather than guess.

## Security model

Adding the `mando` label is full trigger authority: **anyone who can label an issue can start an agent
run**, and the issue's title/description become the agent's prompt. Two consequences to size for:

- **The allowlist is the blast radius.** The repo is validated against `LINEAR_REPO_ALLOWLIST`, so a run can
  only ever touch repos you approved — but a crafted issue *can* steer the inference toward a different
  *allowlisted* repo. Keep the allowlist to repos you're comfortable any labeler running the agent against.
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
| `LINEAR_REPO_ALLOWLIST` | `linear.env` | repos the agent may touch (grounds + validates inference) |
| `LINEAR_DEFAULT_REPO` | `linear.env` | optional fallback repo when inference is unclear |

The API key + webhook secret are Tier-0: host-side only, never injected into a guest VM.

## Troubleshooting

- **401 on deliveries** — the signing secret doesn't match, or a proxy is altering the request body (the
  HMAC is over the raw bytes). Check the secret and that the proxy passes the body through unmodified.
- **Issue not picked up** — check it has the `mando` label, is in a *to-do* state, the repo is in the
  allowlist, and Linear is enabled in Connectors. `journalctl -u mando-connectors` shows the decision.
- **It keeps asking which repo** — the LLM can't map the issue to an allowlisted repo; make the title/first
  line name the repo, add it to the allowlist, or set `LINEAR_DEFAULT_REPO`.
- **Comments don't steer** — the connector needs to resolve its own user id at startup to avoid echoing its
  own comments; if it can't (bad key), it disables comment steering (fail-closed) and logs it. Dispatch
  still works.
- **On a subscription-only box** (no provider API key), the repo resolver has no cheap model to call, so it
  asks on every issue. Use a single-repo allowlist (which skips the LLM) or run with an API-key provider.
