# Telegram setup

Telegram is an optional chat connector (alongside Slack). It runs over the **Bot API with long-polling**
— no public URL or ingress needed, the Telegram analogue of the Slack Socket Mode connector. When it's
configured, the worker posts session updates to a Telegram chat and `telegram-gateway` turns your
messages into tasks and steering.

## What you get

- **Dispatch:** `/mando <owner/repo> <task>` in a chat → starts a session.
- **Steer:** reply in the same chat → the message is delivered to the running session.
- **Attach:** `/mando attach <pr-number|session-id>` / `/mando detach …` for a VS Code session.

## Setup

1. **Create the bot.** Message [@BotFather](https://t.me/BotFather) → `/newbot`, follow the prompts, and
   copy the **HTTP API token** (looks like `123456789:ABCdef…`). Put it in `secrets/telegram-bot-token`
   (or set it from the dashboard's **Connectors → Telegram**).
2. **Let the bot read messages.** `@BotFather → /setprivacy →` your bot `→ Disable`. With privacy mode on,
   a bot only sees commands (`/mando`), not your plain replies — disabling it lets the gateway route your
   follow-up messages. (In a 1:1 chat with the bot this isn't strictly required, but it's simplest.)
3. **Start a chat.** Open a direct chat with your bot, or add it to a group and allow it to read messages.
4. **Deploy.** With `secrets/telegram-bot-token` present, the Ansible control-plane role installs
   `/etc/fleet/telegram.env`, enables `telegram-gateway`, and gives the worker the token so it registers
   the Telegram notifier. (Slack and Telegram can both be enabled at once.)

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
