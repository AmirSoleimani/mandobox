package supervisor

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexArgs(t *testing.T) {
	a := codexArgs(AgentSpec{Prompt: "add a changelog", Model: "gpt-5-codex", SystemPrompt: "be terse"})
	if a[0] != "exec" {
		t.Errorf("first arg = %q, want exec", a[0])
	}
	if !slices.Contains(a, "--model") || !slices.Contains(a, "gpt-5-codex") {
		t.Errorf("model not passed: %v", a)
	}
	last := a[len(a)-1]
	if !strings.HasPrefix(last, "be terse") || !strings.Contains(last, "add a changelog") {
		t.Errorf("instructions should be prepended to the prompt: %q", last)
	}
}

func TestCodexEnv_stripsRealKeysSetsSessionToken(t *testing.T) {
	env := codexEnv(
		[]string{"ANTHROPIC_API_KEY=real-anthropic", "OPENAI_API_KEY=real-openai", "PATH=/usr/bin"},
		AgentSpec{BaseURL: "http://172.31.0.1:8080", AuthToken: "sess-abc"},
	)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "real-anthropic") || strings.Contains(joined, "real-openai") {
		t.Errorf("a real inherited key leaked into the agent env:\n%s", joined)
	}
	for _, want := range []string{
		"OPENAI_API_KEY=sess-abc", // the swappable session token, not a real key
		"OPENAI_BASE_URL=http://172.31.0.1:8080",
		"HTTPS_PROXY=http://172.31.0.1:8080",
		"PATH=/usr/bin", // inherited non-key env preserved
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("env missing %q", want)
		}
	}
}
