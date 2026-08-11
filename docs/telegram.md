# Telegram setup

Telegram is an optional chat connector (alongside Slack). It runs over the **Bot API with long-polling**
— no public URL or ingress needed, the Telegram analogue of the Slack Socket Mode connector. Its inbound
half runs inside the shared **`mando-connectors`** host (the same process that serves Slack); outbound,
the worker posts session updates to your Telegram chat.

## What you get

- **Start / help:** `/start` or `/help` → a short usage reply. (Telegram sends `/start` automatically the
  first time you open the bot, so this is what confirms the bot is alive.)
- **Dispatch:** `/mando <owner/repo> <task>` in a chat → starts a session.
- **Steer:** reply in the same chat → the message is delivered to the running session. In a 1:1 chat, a
  message sent while **no** session is running gets a nudge back with the `/mando` usage instead of silence.
- **Attach:** `/mando attach <pr-number|session-id>` / `/mando detach …` for a VS Code session.

## Setup

1. **Create the bot.** Message [@BotFather](https://t.me/BotFather) → `/newbot`, follow the prompts, and
   copy the **HTTP API token** (looks like `123456789:ABCdef…`). Put it in `secrets/telegram-bot-token`
   (or set it from the dashboard's **Connectors → Telegram**).
2. **Let the bot read messages.** `@BotFather → /setprivacy →` your bot `→ Disable`. With privacy mode on,
   a bot only sees commands (`/mando`, `/start`), not your plain replies — disabling it lets the connector
   route your follow-up steering messages. (In a 1:1 chat with the bot this isn't strictly required, but
   it's simplest.)
3. **Start a chat.** Open a direct chat with your bot, or add it to a group and allow it to read messages.
4. **Enable it.** Two ways:
   - **Dashboard (no redeploy):** paste the token into **Connectors → Telegram** and toggle it **on**. The
     dashboard writes `/etc/fleet/telegram.env`, flips `connectors.json`, and restarts the connector host +
     worker. The Telegram connector then starts inside `mando-connectors` and the worker registers the
     Telegram notifier.
   - **Ansible:** drop the token in `secrets/telegram-bot-token` and run the `control_plane` role — it
     installs `/etc/fleet/telegram.env` and the default `connectors.json`.

   Slack and Telegram both run in the **one** `mando-connectors` process; enable each independently.

## Usage

```
/mando owner/repo add a CHANGELOG.md
/mando --cheap owner/repo fix the lint warnings
```

The bot replies in the chat and opens a thread of updates there; reply to steer, and it will open a PR.

## Threading model (limitation)

Routing is **chat-scoped**: a session's reply address is its chat, so every message in that chat is
delivered to that chat's running session. This means **one active session per chat** — run concurrent
sessions in separate chats. (Per-topic threading in a forum supergroup, to allow many sessions in one
chat, is a possible future enhancement; Slack gets this for free from its first-class threads.)

## Env / secrets

| Secret / env | File on the box | Purpose |
|---|---|---|
| `TELEGRAM_BOT_TOKEN` | `secrets/telegram-bot-token` → `/etc/fleet/telegram.env` | Bot API token (required to enable). |
| `TELEGRAM_DEFAULT_CHAT` | `secrets/telegram-chat` (optional) → `/etc/fleet/telegram.env` | Fallback chat for updates from a non-Telegram dispatch; usually unset. |

## Troubleshooting

| Symptom | Check |
|---|---|
| `/start` gets no reply | The connector isn't running. `journalctl -u mando-connectors -n 20 -o cat` should show `connectors/telegram: connected as @yourbot`. If it says `telegram disabled or unconfigured — skipping`, the token isn't set or Telegram isn't enabled in `connectors.json` — enable it (dashboard **Connectors → Telegram**). |
| Bot replies to `/mando` but not to plain messages | Privacy mode is still on — `@BotFather → /setprivacy → Disable`, then remove and re-add the bot to the chat. |
| Two bots / "conflict" errors in the log | Only one process may long-poll a token. Make sure a stray `mando-connectors` (or an old `telegram-gateway`) isn't also running, and that no Bot API **webhook** is set for the token (`getUpdates` and webhooks are mutually exclusive). |
