package control

import "github.com/chelodo/fleet/internal/supervisor"

// buildMMDS assembles the fleet-agent mmds_payload from launch params. It sets everything the
// guest's BootConfig.validate() requires except `network` and `session_id`, which fleet-agent
// injects at launch (see manager.mergeMMDS). The real Anthropic key is never included — the
// guest gets a per-session LLM auth token routed through the egress gateway (I1, I9, §9).
func buildMMDS(p LaunchParams) map[string]any {
	task := map[string]any{"mode": p.Mode}
	if p.Mode == supervisor.ModeResume {
		task["instructions"] = p.Instructions
	} else {
		task["prompt"] = p.Input.Prompt
	}
	return map[string]any{
		"repo": map[string]any{
			"slug":        p.Input.Repo,
			"clone_url":   p.Input.CloneURL,
			"base_branch": p.Input.BaseBranch,
		},
		"task": task,
		"llm": map[string]any{
			"base_url":   p.Creds.LLMBaseURL,
			"auth_token": p.Creds.LLMAuthToken,
		},
		"github": map[string]any{
			"token":     p.Creds.GitHubToken,
			"bot_user":  p.Creds.GitHubBotUser,
			"bot_email": p.Creds.GitHubBotMail,
		},
		"nats": map[string]any{
			"url":   p.NATSURL,
			"creds": p.Creds.NATSCreds,
		},
		"claude": map[string]any{
			"model": p.Input.Model,
		},
	}
}
