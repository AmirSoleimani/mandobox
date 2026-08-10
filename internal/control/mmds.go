package control

import "github.com/chelodo/mandobox/internal/supervisor"

// buildMMDS assembles the mando-agent mmds_payload from launch params. It sets everything the
// guest's BootConfig.validate() requires except `network` and `session_id`, which mando-agent
// injects at launch (see manager.mergeMMDS). The real Anthropic key is never included — the
// guest gets a per-session LLM auth token routed through the egress gateway.
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
			"head_branch": p.HeadBranch,
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
		"agent": agentMMDS(p),
		"vscode": map[string]any{
			// tunnel_token is deliberately NOT injected here: it's a shared operator credential, so it
			// must not sit in every guest's boot config where any untrusted repo could read it. It's
			// delivered on-demand in the (per-session authenticated) attach command instead.
			"hostname": p.Creds.VSCodeTunnelHostname,
		},
	}
}

// agentMMDS builds the guest's agent config, including operator preamble overrides when set. Empty
// override fields are omitted so the guest falls back to its built-in default preamble.
func agentMMDS(p LaunchParams) map[string]any {
	agent := map[string]any{
		"harness":      p.Input.Agent,
		"instructions": p.Input.Instructions,
	}
	if p.PreambleAutonomous != "" {
		agent["preamble_autonomous"] = p.PreambleAutonomous
	}
	if p.PreambleCollaborate != "" {
		agent["preamble_collaborate"] = p.PreambleCollaborate
	}
	if p.CheapModel != "" {
		agent["cheap_model"] = p.CheapModel
	}
	if p.Auth == "subscription" && p.OAuthToken != "" {
		agent["auth"] = "subscription"
		agent["oauth_token"] = p.OAuthToken
	}
	return agent
}
