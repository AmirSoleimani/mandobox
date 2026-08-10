package supervisor

import (
	"slices"
	"strings"
	"testing"
)

func TestAgentEnvStripsAnthropicAPIKey(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-should-be-removed",
		"HOME=/root",
	}
	env, err := agentEnv(base, AgentSpec{BaseURL: "http://172.31.0.1:8080", AuthToken: "sess-tok"})
	if err != nil {
		t.Fatalf("agentEnv: %v", err)
	}
	for _, e := range env {
		if strings.HasPrefix(strings.ToUpper(e), "ANTHROPIC_API_KEY=") {
			t.Fatalf("security-invariant violation: ANTHROPIC_API_KEY leaked into agent env: %q", e)
		}
	}
	want := []string{
		"ANTHROPIC_BASE_URL=http://172.31.0.1:8080",
		"ANTHROPIC_AUTH_TOKEN=sess-tok",
		"HTTP_PROXY=http://172.31.0.1:8080",
		"HTTPS_PROXY=http://172.31.0.1:8080",
		"NO_PROXY=169.254.169.254,localhost,127.0.0.1",
	}
	for _, w := range want {
		if !slices.Contains(env, w) {
			t.Errorf("agent env missing %q", w)
		}
	}
}

func TestAgentEnvSubscriptionMode(t *testing.T) {
	env, err := agentEnv([]string{"PATH=/usr/bin"}, AgentSpec{
		BaseURL: "http://172.31.0.1:8080", AuthToken: "sess-tok",
		Auth: "subscription", OAuthToken: "sk-ant-oat01-xyz",
	})
	if err != nil {
		t.Fatalf("agentEnv: %v", err)
	}
	// Subscription mode: OAuth token set; NO gateway base URL / auth token; proxy still enforced.
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-xyz") {
		t.Error("subscription mode must set CLAUDE_CODE_OAUTH_TOKEN")
	}
	for _, e := range env {
		if strings.HasPrefix(e, "ANTHROPIC_BASE_URL=") {
			t.Errorf("subscription mode must NOT set ANTHROPIC_BASE_URL (Claude talks to Anthropic directly): %q", e)
		}
		if strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("subscription mode must NOT set ANTHROPIC_AUTH_TOKEN: %q", e)
		}
	}
	if !slices.Contains(env, "HTTPS_PROXY=http://172.31.0.1:8080") {
		t.Error("guest must still egress only through the CONNECT proxy in subscription mode")
	}
}

func TestClaudeArgs(t *testing.T) {
	initial := claudeArgs(AgentSpec{Prompt: "do it", Model: "claude-sonnet-5"})
	if !slices.Contains(initial, "-p") || !slices.Contains(initial, "stream-json") ||
		!slices.Contains(initial, "bypassPermissions") || !slices.Contains(initial, "--model") {
		t.Fatalf("initial args = %v", initial)
	}
	if slices.Contains(initial, "--resume") {
		t.Error("initial run must not pass --resume")
	}

	resume := claudeArgs(AgentSpec{Prompt: "more", Resume: true, ClaudeSessionID: "uuid-1"})
	i := slices.Index(resume, "--resume")
	if i < 0 || resume[i+1] != "uuid-1" {
		t.Fatalf("resume args = %v, want --resume uuid-1", resume)
	}
}
