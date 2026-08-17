package control

import (
	"encoding/json"
	"os"
	"strings"
)

// ProviderID identifies the LLM provider the whole box runs on. Exactly one is active at a time,
// and it governs EVERYTHING — the coding agent AND every helper call (commit messages, intent
// routing). There is deliberately no per-use-case divergence: whatever is enabled, everything runs
// on it. New providers are added to ProviderRegistry; the launch + helper routing are driven off
// the registry fields, not per-provider branches.
type ProviderID string

const (
	ProviderClaudeAPI          ProviderID = "claude_api"
	ProviderClaudeSubscription ProviderID = "claude_subscription"
	ProviderCodex              ProviderID = "codex"
)

// anthropicDirectURL is the real Anthropic endpoint the subscription path talks to directly (it is
// allowlisted in the egress gateway). API-key providers go through the gateway instead, which
// injects the real upstream key host-side.
const anthropicDirectURL = "https://api.anthropic.com"

// ProviderDef is the static definition of a provider: the agent harness it runs, how it
// authenticates, the secret backing it, and its default models.
type ProviderDef struct {
	ID           ProviderID
	Label        string
	Harness      string // supervisor harness id: "claude" | "codex"
	Subscription bool   // true → OAuth token, talk to Anthropic directly; false → gateway→LiteLLM on an API key
	SecretName   string // dashboard-managed secret backing this provider
	Model        string // default main model
	CheapModel   string // default helper model (commit messages, intent routing)
}

// ProviderRegistry is the catalog of supported providers, in display order.
var ProviderRegistry = []ProviderDef{
	{ProviderClaudeAPI, "Claude API", "claude", false, "anthropic-api-key", "claude-sonnet-5", "claude-haiku-4-5-20251001"},
	{ProviderClaudeSubscription, "Claude Subscription", "claude", true, "claude-oauth-token", "claude-sonnet-5", "claude-haiku-4-5-20251001"},
	{ProviderCodex, "Codex", "codex", false, "openai-api-key", "gpt-5-codex", "gpt-5-mini"},
}

func providerDef(id ProviderID) (ProviderDef, bool) {
	for _, d := range ProviderRegistry {
		if d.ID == id {
			return d, true
		}
	}
	return ProviderDef{}, false
}

// providerFile is the on-disk, dashboard-managed selection plus per-provider model overrides.
type providerFile struct {
	Active    ProviderID                       `json:"active"`
	Providers map[ProviderID]providerOverrides `json:"providers,omitempty"`
}

type providerOverrides struct {
	Model      string `json:"model,omitempty"`
	CheapModel string `json:"cheap_model,omitempty"`
}

// ResolvedProvider is the active provider's effective settings for one launch or helper call.
type ResolvedProvider struct {
	ID           ProviderID
	Harness      string
	Subscription bool
	Model        string
	CheapModel   string
	OAuthToken   string // set only when Subscription (read from the token file)
}

// resolveProvider reads the active provider from ProviderConfigPath, applies overrides, and reads
// the OAuth token when the active provider is subscription-based. If the config file is absent it
// falls back to the legacy agent-auth toggle (subscription|api_key) so an in-place upgrade is safe.
func (a *Activities) resolveProvider() ResolvedProvider {
	return resolveProviderFromPaths(a.ProviderConfigPath, a.OAuthTokenPath, a.AuthModePath)
}

// resolveProviderFromPaths is the path-based core of resolveProvider, so a host-side caller outside the
// worker (e.g. a connector) can resolve the same active provider from the same files — see HelperLLMFromPaths.
func resolveProviderFromPaths(providerConfigPath, oauthTokenPath, authModePath string) ResolvedProvider {
	active, overrides := readProviderSelection(providerConfigPath, authModePath)
	def, ok := providerDef(active)
	if !ok {
		def, _ = providerDef(ProviderClaudeAPI) // safe default: gateway + API key
	}
	r := ResolvedProvider{
		ID: def.ID, Harness: def.Harness, Subscription: def.Subscription,
		Model: def.Model, CheapModel: def.CheapModel,
	}
	if ov, ok := overrides[def.ID]; ok {
		if strings.TrimSpace(ov.Model) != "" {
			r.Model = ov.Model
		}
		if strings.TrimSpace(ov.CheapModel) != "" {
			r.CheapModel = ov.CheapModel
		}
	}
	if def.Subscription {
		r.OAuthToken = readTrimmedFile(oauthTokenPath)
	}
	return r
}

func readProviderSelection(providerConfigPath, authModePath string) (ProviderID, map[ProviderID]providerOverrides) {
	if providerConfigPath != "" {
		if raw, err := os.ReadFile(providerConfigPath); err == nil {
			var pf providerFile
			if json.Unmarshal(raw, &pf) == nil && pf.Active != "" {
				return pf.Active, pf.Providers
			}
		}
	}
	// Legacy fallback: the pre-provider agent-auth toggle, so upgrading before the dashboard writes
	// provider.json keeps the current behavior.
	if readTrimmedFile(authModePath) == "subscription" {
		return ProviderClaudeSubscription, nil
	}
	return ProviderClaudeAPI, nil
}

// HelperLLMFromPaths resolves the (baseURL, token, model) a host-side cheap LLM call should use, from the
// same provider config the worker reads — so a separate process (a connector) routes identically:
// subscription → Anthropic direct on the OAuth token; API-key → the gateway. Paths mirror the worker's
// (MANDO_PROVIDER_CONFIG / MANDO_CLAUDE_OAUTH_TOKEN / MANDO_AGENT_AUTH, or the /etc/fleet defaults).
func HelperLLMFromPaths(providerConfigPath, oauthTokenPath, authModePath, gatewayURL string) (baseURL, token, model string) {
	return resolveProviderFromPaths(providerConfigPath, oauthTokenPath, authModePath).helperLLM(gatewayURL)
}

// helperLLM returns the endpoint, bearer token, and model a HOST-SIDE helper call (intent
// classification, run on the worker) should use for the active provider. Subscription talks to
// Anthropic directly on the OAuth token; API-key providers go through the gateway (which injects
// the real key, so the placeholder bearer is fine).
func (r ResolvedProvider) helperLLM(gatewayURL string) (baseURL, token, model string) {
	if r.Subscription && r.OAuthToken != "" {
		return anthropicDirectURL, r.OAuthToken, r.CheapModel
	}
	return strings.TrimRight(gatewayURL, "/"), "classify", r.CheapModel
}
