package main

// connectors.go groups the box's integrations (GitHub, Slack, …) into one place, so an operator
// configures a connector and its keys together instead of hunting through the flat Secrets list.
// Adding a connector later (Linear, Jira, Discord) is a registry entry here plus its secrets in
// secrets.go — the view and the UI are driven off this registry, nothing per-connector is hardcoded.

import (
	"encoding/json"
	"os"
)

type connectorDef struct {
	ID      string
	Label   string
	Blurb   string
	Secrets []string // managed-secret names this connector needs (see managedSecrets)
	Steps   []string // short setup guide, shown inline on the card
	Doc     string   // repo-relative full guide (referenced, not fetched)
}

func connectorDefs() []connectorDef {
	return []connectorDef{
		{
			ID:      "github",
			Label:   "GitHub",
			Blurb:   "Clone repos, push branches, and open/update PRs on your behalf. Delivers PR reviews, comments, and CI status back to the agent via webhooks.",
			Secrets: []string{"github-app-key", "webhook-secret"},
			Steps: []string{
				"Prerequisite: the target repos must live under a GitHub organization — a GitHub App on a personal account can't open PRs. Move them to your org first.",
				"Create a GitHub App (Org → Settings → Developer settings → GitHub Apps → New). Repository permissions — grant ONLY: Contents (Read & write), Pull requests (Read & write), Checks (Read-only). Do NOT grant actions, administration, workflows, or any org-level permission.",
				"Subscribe to webhook events: pull_request, pull_request_review, pull_request_review_comment, issue_comment, check_suite.",
				"Set a webhook secret on the App and paste the same value into “GitHub webhook secret” below. (Its webhook URL points at webhook-rx; it can stay inactive until you expose that receiver — Slack-only flows don't need it.)",
				"Generate a private key (PEM) and paste it into “GitHub App private key” below.",
				"Install the App on the specific repos the agent should work on — not “All repositories”.",
				"Safety (enforced by GitHub, not our code): protect main — require a PR and 1 approving review, and exclude the App from approving its own PRs.",
				"Safety: add a ruleset allowing the App to push only to agent/* branches. With main protected, agent output can never reach main directly.",
			},
			Doc: "docs/github-setup.md",
		},
		{
			ID:      "slack",
			Label:   "Slack",
			Blurb:   "Post session updates to a channel and take commands from threads over Socket Mode — run /mando in Slack to dispatch a task.",
			Secrets: []string{"slack-bot-token", "slack-app-token"},
			Steps: []string{
				"Create a Slack app at api.slack.com/apps → From scratch, in your workspace.",
				"Enable Socket Mode (no public URL needed) — it generates an App-Level Token (xapp-, scope connections:write). Paste it into “Slack app token” below.",
				"OAuth & Permissions → Bot Token Scopes — add: chat:write (post the thread), commands (receive /mando), channels:history + groups:history (read thread replies), files:read (read files dropped in a thread), and optionally app_mentions:read.",
				"Slash Commands → Create New Command “/mando” (the Request URL can be anything — Socket Mode ignores it).",
				"Event Subscriptions → Enable Events → Subscribe to bot events: message.channels (and message.groups for private channels).",
				"Install to Workspace → copy the Bot User OAuth Token (xoxb-) into “Slack bot token” below.",
				"Invite the bot into a channel (/invite @your-app) — it only sees channels it's a member of — then run “/mando owner/repo <task>”.",
			},
			Doc: "docs/slack.md",
		},
		{
			ID:      "telegram",
			Label:   "Telegram",
			Blurb:   "Post session updates to a Telegram chat and take commands from it — send /mando to dispatch a task. Runs over the Bot API (long-poll, no public URL).",
			Secrets: []string{"telegram-bot-token"},
			Steps: []string{
				"Message @BotFather → /newbot, follow the prompts, and copy the HTTP API token into “Telegram bot token” below.",
				"@BotFather → /setprivacy → your bot → Disable, so it can read your plain replies (needed to steer a session). A 1:1 chat doesn't strictly need this.",
				"Start a chat with the bot (or add it to a group and let it read messages).",
				"Send “/mando owner/repo <task>” in that chat to dispatch; reply in the chat to steer. Use “/mando --cheap …” for the cheap model.",
				"Routing is chat-scoped — one active session per chat; use separate chats for concurrent sessions.",
			},
			Doc: "docs/telegram.md",
		},
	}
}

type connectorSecretView struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Hint        string `json:"hint,omitempty"`
	Desc        string `json:"desc,omitempty"`
	Present     bool   `json:"present"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type connectorView struct {
	ID        string                `json:"id"`
	Label     string                `json:"label"`
	Blurb     string                `json:"blurb"`
	Connected bool                  `json:"connected"` // true when every required secret is present
	Enabled   bool                  `json:"enabled"`   // whether it should run (connectors.json)
	Secrets   []connectorSecretView `json:"secrets"`
	Steps     []string              `json:"steps,omitempty"`
	Doc       string                `json:"doc,omitempty"`
}

type connectorStore struct {
	secrets    *secretStore
	configPath string // connectors.json — the enable/disable config shared with the host + worker
}

func newConnectorStore(secrets *secretStore, configPath string) *connectorStore {
	return &connectorStore{secrets: secrets, configPath: configPath}
}

type connectorEnable struct {
	Enabled bool `json:"enabled"`
}

// loadConfig reads connectors.json ({"slack":{"enabled":true},…}); missing/invalid → empty.
func (c *connectorStore) loadConfig() map[string]connectorEnable {
	b, err := os.ReadFile(c.configPath)
	if err != nil {
		return map[string]connectorEnable{}
	}
	var m map[string]connectorEnable
	if json.Unmarshal(b, &m) != nil {
		return map[string]connectorEnable{}
	}
	return m
}

// setEnabled writes id's enabled flag into connectors.json (atomically), preserving other entries.
func (c *connectorStore) setEnabled(id string, enabled bool) error {
	m := c.loadConfig()
	m[id] = connectorEnable{Enabled: enabled}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.configPath + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.configPath)
}

func (c *connectorStore) view() []connectorView {
	cfg := c.loadConfig()
	out := make([]connectorView, 0, len(connectorDefs()))
	for _, d := range connectorDefs() {
		cv := connectorView{ID: d.ID, Label: d.Label, Blurb: d.Blurb, Connected: len(d.Secrets) > 0, Steps: d.Steps, Doc: d.Doc}
		for _, name := range d.Secrets {
			s := connectorSecretView{Name: name}
			if sv, ok := c.secrets.oneView(name); ok {
				s.Label, s.Hint, s.Desc = sv.Label, sv.Hint, sv.Desc
				s.Present, s.Fingerprint = sv.Present, sv.Fingerprint
			}
			if !s.Present {
				cv.Connected = false
			}
			cv.Secrets = append(cv.Secrets, s)
		}
		// Enabled: an explicit connectors.json entry wins; absent = on when configured (matching the
		// connector host + worker default).
		if e, ok := cfg[d.ID]; ok {
			cv.Enabled = e.Enabled
		} else {
			cv.Enabled = cv.Connected
		}
		out = append(out, cv)
	}
	return out
}
