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
			t.Fatalf("I9 violation: ANTHROPIC_API_KEY leaked into agent env: %q", e)
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

func TestClaudeArgs(t *testing.T) {
	initial := claudeArgs(AgentSpec{Prompt: "do it", Model: "claude-sonnet-5"})
	if !slices.Contains(initial, "-p") || !slices.Contains(initial, "stream-json") ||
		!slices.Contains(initial, "acceptEdits") || !slices.Contains(initial, "--model") {
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
