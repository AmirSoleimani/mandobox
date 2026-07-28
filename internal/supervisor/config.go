// Package supervisor implements fc-supervisor, the guest's PID 1. It reads its boot
// configuration and per-session credentials from MMDS, runs Claude Code against the target
// repo behind a host-side LLM gateway, and reports progress over NATS (PLAN §8).
//
// Trust: everything here runs inside the untrusted guest. The only credentials present are
// per-session Tier-1 tokens delivered via MMDS; no real key is ever here (I1), and
// ANTHROPIC_API_KEY is never set (I9).
package supervisor

import (
	"encoding/json"
	"fmt"

	"github.com/chelodo/fleet/internal/session"
)

// BootConfig is the guest's boot configuration, parsed from the MMDS JSON that fleet-agent
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
}

// NetworkConfig is the point-to-point link fleet-agent allocated (§8.1: configure eth0
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
	CloneURL   string `json:"clone_url"` // https clone URL (no embedded token — §9)
	BaseBranch string `json:"base_branch"`
}

// TaskConfig carries the work to do. Mode is "initial" (first run: implement + open PR) or
// "resume" (later round: apply instructions, push to the same branch, do NOT open a new PR).
type TaskConfig struct {
	Mode         string   `json:"mode"`
	Prompt       string   `json:"prompt"`
	Instructions []string `json:"instructions"`
}

// LLMConfig points Claude Code at the host-side gateway. AuthToken is a per-session bearer
// token the gateway swaps for the real key (§4.5, §10).
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

// NATSConfig is the control-plane transport (§4.4). Creds is a session-scoped JWT.
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
	case ModeInitial, ModeResume:
	default:
		return fmt.Errorf("boot config: task.mode must be %q or %q, got %q", ModeInitial, ModeResume, c.Task.Mode)
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

// Branch returns the git branch for this session: agent/<session_id> (§5).
func (c BootConfig) Branch() string { return c.SessionID.Branch() }
