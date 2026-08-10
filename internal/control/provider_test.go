package control

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProvider writes an Activities wired to temp files for provider.json + the OAuth token.
func testActivities(t *testing.T, providerJSON, authMode, oauthToken string) *Activities {
	t.Helper()
	dir := t.TempDir()
	a := &Activities{GatewayURL: "http://gw:8080"}
	if providerJSON != "" {
		p := filepath.Join(dir, "provider.json")
		if err := os.WriteFile(p, []byte(providerJSON), 0o600); err != nil {
			t.Fatal(err)
		}
		a.ProviderConfigPath = p
	}
	if authMode != "" {
		p := filepath.Join(dir, "agent-auth")
		if err := os.WriteFile(p, []byte(authMode), 0o600); err != nil {
			t.Fatal(err)
		}
		a.AuthModePath = p
	}
	if oauthToken != "" {
		p := filepath.Join(dir, "oauth")
		if err := os.WriteFile(p, []byte(oauthToken), 0o600); err != nil {
			t.Fatal(err)
		}
		a.OAuthTokenPath = p
	}
	return a
}

func TestResolveProviderSubscription(t *testing.T) {
	a := testActivities(t, `{"active":"claude_subscription"}`, "", "sk-ant-oat01-secret")
	r := a.resolveProvider()
	if r.ID != ProviderClaudeSubscription || !r.Subscription {
		t.Fatalf("want subscription, got %+v", r)
	}
	if r.Harness != "claude" || r.Model != "claude-sonnet-5" || r.CheapModel != "claude-haiku-4-5-20251001" {
		t.Fatalf("registry defaults not applied: %+v", r)
	}
	if r.OAuthToken != "sk-ant-oat01-secret" {
		t.Fatalf("oauth token not read: %q", r.OAuthToken)
	}
	// Subscription helper calls go straight to Anthropic on the token.
	base, tok, model := r.helperLLM(a.GatewayURL)
	if base != anthropicDirectURL || tok != "sk-ant-oat01-secret" || model != "claude-haiku-4-5-20251001" {
		t.Fatalf("subscription helperLLM wrong: %s / %s / %s", base, tok, model)
	}
}

func TestResolveProviderAPIAndOverrides(t *testing.T) {
	a := testActivities(t, `{"active":"claude_api","providers":{"claude_api":{"model":"claude-opus-4-8","cheap_model":"claude-haiku-4-5-20251001"}}}`, "", "")
	r := a.resolveProvider()
	if r.ID != ProviderClaudeAPI || r.Subscription {
		t.Fatalf("want claude_api non-subscription, got %+v", r)
	}
	if r.Model != "claude-opus-4-8" {
		t.Fatalf("override model not applied: %q", r.Model)
	}
	// API-key helper calls go through the gateway (placeholder bearer; gateway injects the key).
	base, tok, _ := r.helperLLM(a.GatewayURL)
	if base != "http://gw:8080" || tok != "classify" {
		t.Fatalf("api-key helperLLM wrong: %s / %s", base, tok)
	}
}

func TestResolveProviderCodex(t *testing.T) {
	a := testActivities(t, `{"active":"codex"}`, "", "")
	r := a.resolveProvider()
	if r.Harness != "codex" || r.Model != "gpt-5-codex" || r.CheapModel != "gpt-5-mini" {
		t.Fatalf("codex registry defaults wrong: %+v", r)
	}
}

func TestResolveProviderLegacyFallback(t *testing.T) {
	// No provider.json → fall back to the legacy agent-auth toggle.
	a := testActivities(t, "", "subscription", "legacy-tok")
	r := a.resolveProvider()
	if r.ID != ProviderClaudeSubscription || r.OAuthToken != "legacy-tok" {
		t.Fatalf("legacy subscription fallback failed: %+v", r)
	}
	// Absent everything → safe default is claude_api.
	b := testActivities(t, "", "", "")
	if got := b.resolveProvider(); got.ID != ProviderClaudeAPI {
		t.Fatalf("empty default should be claude_api, got %s", got.ID)
	}
}
