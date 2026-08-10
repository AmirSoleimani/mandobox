package main

// confighelp.go serves a static, hand-authored reference for the box config editor. It mirrors
// internal/control's DefaultBoxConfig + mandobox.example.yml (kept in sync manually) so the operator
// gets key descriptions, defaults, and insertable snippets without the dashboard importing the
// control plane. This is guidance only — the control plane remains the validator of record.

type configKeyHelp struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Default string `json:"default"`
	Allowed string `json:"allowed"`
	Desc    string `json:"desc"`
}

type configSnippet struct {
	Label string `json:"label"`
	YAML  string `json:"yaml"`
}

type configHelp struct {
	Keys     []configKeyHelp `json:"keys"`
	Snippets []configSnippet `json:"snippets"`
	Example  string          `json:"example"`
}

func configHelpDoc() configHelp {
	return configHelp{
		Keys: []configKeyHelp{
			{"defaults.agent", "string", "claude", "any in limits.agents_allowed", "Harness every session uses unless a repo overrides it."},
			{"defaults.model", "string", "claude-sonnet-5", "any in limits.models_allowed", "Model every session uses unless overridden."},
			{"defaults.resources", "string", "medium", "a profile in limits.resources.profiles", "Default VM size profile."},
			{"defaults.keep_alive", "duration", "15m", "e.g. 5m, 30m, 2h", "How long a warm VM idles before parking."},
			{"defaults.review.max_rounds", "int", "5", "≥ 1", "Automated review rounds before stopping."},
			{"limits.resources.profiles", "map", "small/medium/large", "vcpus, mem (MiB), disk (MiB)", "Named VM size profiles a repo may request."},
			{"limits.resources.max_profile", "string", "large", "a profile name", "Largest profile a repo's .mandobox.yml may request."},
			{"limits.cost_ceiling_usd", "number", "15", "> 0", "Per-session hard spend cap (operator-only)."},
			{"limits.hard_ttl", "duration", "336h", "e.g. 168h, 336h", "Max workflow lifetime, bounding a long-lived PR."},
			{"limits.concurrency", "int", "8", "≥ 1", "Max concurrent sessions (operator-only)."},
			{"limits.agents_allowed", "list", "[claude]", "claude, codex", "Harnesses a repo's .mandobox.yml may pick."},
			{"limits.models_allowed", "list", "[claude-sonnet-5, claude-haiku-4-5-20251001]", "model IDs", "Models a repo may pick."},
		},
		Snippets: []configSnippet{
			{"defaults", "defaults:\n  agent: claude\n  model: claude-sonnet-5\n  resources: medium\n  keep_alive: 15m\n  review:\n    max_rounds: 5\n"},
			{"resource profiles", "limits:\n  resources:\n    profiles:\n      small:  { vcpus: 1, mem: 2048, disk: 4096 }\n      medium: { vcpus: 2, mem: 4096, disk: 8192 }\n      large:  { vcpus: 4, mem: 8192, disk: 16384 }\n    max_profile: large\n"},
			{"limits", "limits:\n  cost_ceiling_usd: 15\n  hard_ttl: 336h\n  concurrency: 8\n  agents_allowed: [claude]\n  models_allowed: [claude-sonnet-5, claude-haiku-4-5-20251001]\n"},
			{"enable Codex", "limits:\n  agents_allowed: [claude, codex]\n  models_allowed: [claude-sonnet-5, claude-haiku-4-5-20251001, gpt-5-codex]\n"},
		},
		Example: configExample,
	}
}

const configExample = `# Operator config for mandobox — /etc/fleet/mandobox.yml. Everything is optional; omitted keys fall
# back to built-in defaults. Box-wide DEFAULTS every session inherits, and LIMITS a repo's committed
# .mandobox.yml is clamped to (over-limit requests are adjusted and the repo author is warned).

defaults:
  agent: claude                 # claude | codex | …  (must be in limits.agents_allowed)
  model: claude-sonnet-5        # must be in limits.models_allowed
  resources: medium             # a profile name from limits.resources.profiles
  keep_alive: 15m               # how long a warm VM idles before parking
  review:
    max_rounds: 5

limits:
  resources:
    profiles:
      small:  { vcpus: 1, mem: 2048, disk: 4096 }    # mem/disk in MiB
      medium: { vcpus: 2, mem: 4096, disk: 8192 }
      large:  { vcpus: 4, mem: 8192, disk: 16384 }
    max_profile: large          # a repo may not request a bigger profile than this
  cost_ceiling_usd: 15          # per-session hard cap (operator-only)
  hard_ttl: 336h                # 14 days
  concurrency: 8                # operator-only
  agents_allowed: [claude]      # which harnesses a repo's .mandobox.yml may pick
  models_allowed: [claude-sonnet-5, claude-haiku-4-5-20251001]
`
