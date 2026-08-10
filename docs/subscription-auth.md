# Using your Claude subscription instead of an API key

By default, Mandobox authenticates the agent with an **Anthropic API key**, injected host-side by the
egress gateway so the guest VM never sees it (that's what keeps things safe for parallel, and
potentially untrusted, work). You pay per token.

If you run Mandobox as a **single user on your own box, for your own repos**, you can instead point
the agent at your **Claude Pro/Max subscription** — flat-rate, no per-token bill. This page is the
how-to.

> ⚠️ **Single-user only.** Anthropic's Usage Policy allows *your own ordinary use* of Claude Code on
> your subscription (headless included — the `setup-token` flow exists for exactly this). It does
> **not** permit routing **other people's** requests through your subscription credentials, offering
> it as a product with logins, or shared/at-scale automation — those require API keys. So use
> subscription mode only for **yourself**. If Mandobox ever serves other people or teams, switch that
> back to an API key. See Anthropic's
> [Usage Policy](https://privacy.claude.com/en/articles/9301722-updates-to-our-acceptable-use-policy-now-usage-policy-consumer-terms-of-service-and-privacy-policy),
> [Use Claude Code with your Pro/Max plan](https://support.claude.com/en/articles/11145838-use-claude-code-with-your-pro-or-max-plan),
> and [Claude Code auth docs](https://code.claude.com/docs/en/authentication). (This is guidance, not
> legal advice — re-check the terms yourself, and again if your usage changes.)

## What you trade off

Subscription mode bypasses the gateway/LiteLLM for the model traffic, so compared to the default:

- **The token lives inside the VM.** It's how Claude Code authenticates. Treat it as sensitive and
  use this mode **only for repos you trust** — never for untrusted third-party code.
- **No per-session cost figures.** It's flat-rate, so the dashboard's **Costs** tab has nothing to
  attribute for these sessions.
- **No model routing / no Codex** on these sessions — Claude, on your plan, only.
- **Low concurrency.** A Pro/Max plan is metered for one person; a few parallel sessions is realistic,
  a busy fleet will hit your plan's limits.

For anything multi-user or scaled, keep the API-key default.

## Step 1 — Mint a long-lived token

On a machine with a browser, logged into your Claude plan, with Claude Code installed:

```
claude setup-token
```

It opens a browser to authorize, then prints a **long-lived OAuth token** starting `sk-ant-oat01-…`
(valid ~1 year, tied to your plan). It's shown **once** — copy it.

## Step 2 — Install the token on the box

Either drop it into the gitignored secrets so the deploy installs it host-side:

```
printf '%s' 'sk-ant-oat01-…' > secrets/claude-oauth-token
ansible-playbook -i inventory/local.yml deploy.yml --tags control_plane   # installs /etc/fleet/claude-oauth-token
```

…or set/rotate it later from the dashboard's **Secrets** tab (the *Claude subscription token* entry).

## Step 3 — Enable subscription mode

In the dashboard, open **Config → Agent auth** and switch the toggle to **Subscription**. (It writes
`/etc/fleet/agent-auth`, which the worker re-reads on every new task — so it takes effect on the next
dispatch, no restart.) The toggle shows a red flag if the token isn't installed yet.

Prefer the command line? Set the mode directly on the box:

```
printf 'subscription' > /etc/fleet/agent-auth   # or 'api_key' to switch back
```

## Step 4 — Verify

Start a new session (dashboard **+ New session**, or Slack) and open its console. It should run
on your subscription — you'll see the agent work as usual, and your Anthropic account's usage tick up
instead of an API bill.

## Turning it off

Flip the toggle back to **API key** (or `printf 'api_key' > /etc/fleet/agent-auth`). New sessions use
the gateway + API key again immediately. Existing sessions keep the mode they launched with.

## Renewing the token

The token lasts ~1 year. Re-run `claude setup-token`, then update it in **Secrets** (or rewrite
`secrets/claude-oauth-token` and re-deploy). Revoke a token anytime from your Anthropic account.

## How it works (for the curious)

When the mode is `subscription`, the worker reads `/etc/fleet/agent-auth` and the token from
`/etc/fleet/claude-oauth-token` at launch and passes them to the guest. Inside the VM, the supervisor
sets `CLAUDE_CODE_OAUTH_TOKEN` and **omits** `ANTHROPIC_BASE_URL`, so Claude Code authenticates on
your plan and talks to `api.anthropic.com` **directly** — still only through the host CONNECT proxy,
which allowlists that host. The API-key path (gateway + per-session token, real key never in the
guest) is untouched and remains the default.
