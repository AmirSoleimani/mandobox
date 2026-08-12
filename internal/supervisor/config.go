// Package supervisor implements fc-supervisor, the guest's PID 1. It reads its boot
// configuration and per-session credentials from MMDS, runs Claude Code against the target
// repo behind a host-side LLM gateway, and reports progress over NATS.
//
// Trust: everything here runs inside the untrusted guest. The only credentials present are
// per-session Tier-1 tokens delivered via MMDS; no real key is ever here, and
// ANTHROPIC_API_KEY is never set.
package supervisor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AmirSoleimani/mandobox/internal/session"
)

// BootConfig is the guest's boot configuration, parsed from the MMDS JSON that mando-agent
// injected (network + session_id) merged with the control plane's payload (repo, task,
// gateway, github, nats). See docs for the schema.
type BootConfig struct {
	SessionID session.ID    `json:"session_id"`
	Network   NetworkConfig `json:"network"`
	Repo      RepoConfig    `json:"repo"`
	Task      TaskConfig    `json:"task"`
	LLM       LLMConfig     `json:"llm"`
	GitHub    GitHubConfig  `json:"github"`
	NATS      NATSConfig    `json:"nats"`
	Claude    ClaudeConfig  `json:"claude"`
	Agent     AgentConfig   `json:"agent,omitempty"`
	VSCode    VSCodeConfig  `json:"vscode,omitempty"`
}

// AgentConfig selects the coding-agent harness and carries per-repo instructions from the resolved
// .mandobox.yml. Harness "" defaults to Claude Code. Instructions are appended to the agent's system
// prompt (they don't replace the repo's own CLAUDE.md/AGENTS.md).
type AgentConfig struct {
	Harness      string `json:"harness,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// PreambleAutonomous / PreambleCollaborate / PreamblePlan override the built-in task preambles (the
	// base agent system prompts) when the operator sets them box-side. Empty → the built-in default is
	// used. PreamblePlan is the plan-mode preamble (explore + write a plan, don't build).
	PreambleAutonomous  string `json:"preamble_autonomous,omitempty"`
	PreambleCollaborate string `json:"preamble_collaborate,omitempty"`
	PreamblePlan        string `json:"preamble_plan,omitempty"`
	// Auth selects how the agent reaches the LLM. "" / "api_key" (default) → via the host gateway on a
	// per-session token; the real key stays host-side. "subscription" → Claude Code authenticates on
	// the operator's Claude plan with OAuthToken, talking to Anthropic directly (single-user only; the
	// token lives in the guest). See docs/subscription-auth.md.
	Auth       string `json:"auth,omitempty"`
	OAuthToken string `json:"oauth_token,omitempty"`
	// CheapModel is the active provider's helper model (commit messages). It follows the same provider
	// as the agent so helpers never diverge onto a different provider/key. Empty → the built-in default.
	CheapModel string `json:"cheap_model,omitempty"`
}

// VSCodeConfig carries a pre-authenticated `code tunnel` token (the contents of the CLI's
// token.json) so a human attach skips the GitHub device login, plus the hostname that token was
// minted under. The CLI binds its stored auth to the hostname, so the guest adopts Hostname before
// starting the tunnel or the token reads as logged-out. Both optional.
type VSCodeConfig struct {
	TunnelToken string `json:"tunnel_token,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
}

// NetworkConfig is the point-to-point link mando-agent allocated (configure eth0
// statically, no DHCP).
type NetworkConfig struct {
	Tap       string `json:"tap"`
	HostIP    string `json:"host_ip"`
	GuestIP   string `json:"guest_ip"`
	PrefixLen int    `json:"prefix_len"`
	Gateway   string `json:"gateway"`
	DNS       string `json:"dns"`
}

// RepoConfig identifies the repository to work on.
type RepoConfig struct {
	Slug       string `json:"slug"`      // owner/name
	CloneURL   string `json:"clone_url"` // https clone URL (no embedded token)
	BaseBranch string `json:"base_branch"`
	// HeadBranch is the agent branch to push/PR, chosen by the control plane (a task-derived name).
	// Empty → fall back to the session-derived default (see Branch).
	HeadBranch string `json:"head_branch,omitempty"`
}

// TaskConfig carries the work to do. Mode is one of the Mode* constants: "initial" (first run:
// implement + open PR), "resume" (later round: apply instructions, push to the same branch, do NOT
// open a new PR), "plan" (explore + write a plan, never commit — pauses for human approval), or
// "execute" (build the agreed plan autonomously, resuming the plan transcript, and open the PR).
type TaskConfig struct {
	Mode         string   `json:"mode"`
	Prompt       string   `json:"prompt"`
	Instructions []string `json:"instructions"`
}

// LLMConfig points Claude Code at the host-side gateway. AuthToken is a per-session bearer
// token the gateway swaps for the real key.
type LLMConfig struct {
	BaseURL   string `json:"base_url"`
	AuthToken string `json:"auth_token"`
}

// GitHubConfig holds the Tier-1 installation token and the App bot identity for commits.
type GitHubConfig struct {
	Token    string `json:"token"`
	BotUser  string `json:"bot_user"`
	BotEmail string `json:"bot_email"`
}

// NATSConfig is the control-plane transport. Creds is a session-scoped JWT.
type NATSConfig struct {
	URL   string `json:"url"`
	Creds string `json:"creds"`
}

// ClaudeConfig pins the model and (optionally) an expected CLI version.
type ClaudeConfig struct {
	Model   string `json:"model"`
	Version string `json:"version"`
}

// Modes.
const (
	ModeInitial = "initial"
	ModeResume  = "resume"
	// ModePlan explores the repo and writes a plan (to the .mando/needs-input.md sentinel) without
	// editing code; the turn pauses for human review. ModeExecute then builds the agreed plan
	// autonomously — like ModeInitial but resuming the plan transcript — and opens the PR.
	ModePlan    = "plan"
	ModeExecute = "execute"
)

// ParseBootConfig decodes and validates the MMDS JSON.
func ParseBootConfig(data []byte) (BootConfig, error) {
	var c BootConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return BootConfig{}, fmt.Errorf("parse boot config: %w", err)
	}
	if err := c.validate(); err != nil {
		return BootConfig{}, err
	}
	return c, nil
}

func (c BootConfig) validate() error {
	if !c.SessionID.Valid() {
		return fmt.Errorf("boot config: invalid session_id %q", c.SessionID)
	}
	switch c.Task.Mode {
	case ModeInitial, ModeResume, ModePlan, ModeExecute:
	default:
		return fmt.Errorf("boot config: task.mode must be one of %q/%q/%q/%q, got %q",
			ModeInitial, ModeResume, ModePlan, ModeExecute, c.Task.Mode)
	}
	for field, v := range map[string]string{
		"repo.clone_url":   c.Repo.CloneURL,
		"repo.base_branch": c.Repo.BaseBranch,
		"llm.base_url":     c.LLM.BaseURL,
		"llm.auth_token":   c.LLM.AuthToken,
		"github.token":     c.GitHub.Token,
		"nats.url":         c.NATS.URL,
		"network.guest_ip": c.Network.GuestIP,
		"network.gateway":  c.Network.Gateway,
	} {
		if v == "" {
			return fmt.Errorf("boot config: %s is required", field)
		}
	}
	return nil
}

// Branch returns the git branch the agent pushes to: the control plane's task-derived head_branch
// when set, else the session-derived default agent/<session_id>.
func (c BootConfig) Branch() string {
	if b := strings.TrimSpace(c.Repo.HeadBranch); b != "" {
		return b
	}
	return c.SessionID.Branch()
}
