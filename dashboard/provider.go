package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// provider.go is the single place to choose the LLM provider the whole box runs on and give it its
// secret. Exactly one provider is active, and it governs EVERYTHING — the agent AND every helper
// call (commit messages, intent routing). No per-use-case divergence.
//
// The worker reads the same provider.json (internal/control/provider.go). This registry mirrors the
// control-plane one by hand (the dashboard is a separate module) — keep the two in sync.

type providerDef struct {
	ID           string
	Label        string
	Harness      string
	Subscription bool
	SecretName   string // managed secret backing this provider (secrets.go)
	Blurb        string
	Models       []string // selectable main models (first = default)
	CheapModels  []string // selectable helper models (first = default)
}

func providerDefs() []providerDef {
	return []providerDef{
		{
			ID: "claude_api", Label: "Claude API", Harness: "claude", Subscription: false,
			SecretName:  "anthropic-api-key",
			Blurb:       "Anthropic API key. The agent and every helper route through the gateway → LiteLLM; the real key never enters a VM. Per-token billing; fine for multiple users or scale.",
			Models:      []string{"claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5-20251001"},
			CheapModels: []string{"claude-haiku-4-5-20251001", "claude-sonnet-5"},
		},
		{
			ID: "claude_subscription", Label: "Claude Subscription", Harness: "claude", Subscription: true,
			SecretName:  "claude-oauth-token",
			Blurb:       "Your own Claude plan via `claude setup-token`. Flat-rate, single-user only — the token runs inside the VM and helper calls use it too (nothing falls back to an API key). See docs/subscription-auth.md.",
			Models:      []string{"claude-sonnet-5", "claude-opus-4-8", "claude-haiku-4-5-20251001"},
			CheapModels: []string{"claude-haiku-4-5-20251001", "claude-sonnet-5"},
		},
		{
			ID: "codex", Label: "Codex (OpenAI)", Harness: "codex", Subscription: false,
			SecretName:  "openai-api-key",
			Blurb:       "OpenAI's Codex harness via LiteLLM. The agent and helpers run on your OpenAI key.",
			Models:      []string{"gpt-5-codex", "gpt-5"},
			CheapModels: []string{"gpt-5-mini", "gpt-5"},
		},
	}
}

// providerFileOverrides / providerFile mirror the on-disk provider.json the worker reads.
type providerFileOverrides struct {
	Model      string `json:"model,omitempty"`
	CheapModel string `json:"cheap_model,omitempty"`
}

type providerFile struct {
	Active    string                           `json:"active"`
	Providers map[string]providerFileOverrides `json:"providers,omitempty"`
}

type providerStore struct {
	path    string
	secrets *secretStore
}

func newProviderStore(path string, secrets *secretStore) *providerStore {
	return &providerStore{path: path, secrets: secrets}
}

func (p *providerStore) read() providerFile {
	pc := providerFile{Active: "claude_api", Providers: map[string]providerFileOverrides{}}
	if b, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(b, &pc)
	}
	if pc.Providers == nil {
		pc.Providers = map[string]providerFileOverrides{}
	}
	if pc.Active == "" {
		pc.Active = "claude_api"
	}
	return pc
}

func (p *providerStore) write(pc providerFile) error {
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(pc, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

type providerItemView struct {
	ID                string   `json:"id"`
	Label             string   `json:"label"`
	Harness           string   `json:"harness"`
	Subscription      bool     `json:"subscription"`
	Blurb             string   `json:"blurb"`
	Active            bool     `json:"active"`
	Ready             bool     `json:"ready"` // secret present → can be activated
	SecretName        string   `json:"secret_name"`
	SecretLabel       string   `json:"secret_label"`
	SecretHint        string   `json:"secret_hint"`
	SecretPresent     bool     `json:"secret_present"`
	SecretFingerprint string   `json:"secret_fingerprint,omitempty"`
	Models            []string `json:"models"`
	CheapModels       []string `json:"cheap_models"`
	Model             string   `json:"model"`
	CheapModel        string   `json:"cheap_model"`
}

type providerView struct {
	Active    string             `json:"active"`
	Providers []providerItemView `json:"providers"`
}

func (p *providerStore) view() providerView {
	pc := p.read()
	out := providerView{Active: pc.Active}
	for _, d := range providerDefs() {
		ov := pc.Providers[d.ID]
		model := ov.Model
		if model == "" && len(d.Models) > 0 {
			model = d.Models[0]
		}
		cheap := ov.CheapModel
		if cheap == "" && len(d.CheapModels) > 0 {
			cheap = d.CheapModels[0]
		}
		it := providerItemView{
			ID: d.ID, Label: d.Label, Harness: d.Harness, Subscription: d.Subscription, Blurb: d.Blurb,
			Active: pc.Active == d.ID, SecretName: d.SecretName,
			Models: d.Models, CheapModels: d.CheapModels, Model: model, CheapModel: cheap,
		}
		if sv, ok := p.secrets.oneView(d.SecretName); ok {
			it.SecretLabel = sv.Label
			it.SecretHint = sv.Hint
			it.SecretPresent = sv.Present
			it.SecretFingerprint = sv.Fingerprint
		}
		it.Ready = it.SecretPresent
		out.Providers = append(out.Providers, it)
	}
	return out
}

func (p *providerStore) def(id string) (providerDef, bool) {
	for _, d := range providerDefs() {
		if d.ID == id {
			return d, true
		}
	}
	return providerDef{}, false
}

// activate switches the active provider. Guard: its secret must be present, so the box never
// activates a provider that can't authenticate.
func (p *providerStore) activate(id string) (providerView, error) {
	d, ok := p.def(id)
	if !ok {
		return p.view(), fmt.Errorf("unknown provider %q", id)
	}
	if sv, ok := p.secrets.oneView(d.SecretName); !ok || !sv.Present {
		return p.view(), fmt.Errorf("set the %s secret first", d.Label)
	}
	pc := p.read()
	pc.Active = id
	if err := p.write(pc); err != nil {
		return p.view(), err
	}
	return p.view(), nil
}

// setModel records the chosen main/helper model for a provider. Model ids are free-form so a custom
// identifier (a new/preview model, a fine-tune) is accepted — only a light charset sanity check runs.
func (p *providerStore) setModel(id, model, cheap string) (providerView, error) {
	if _, ok := p.def(id); !ok {
		return p.view(), fmt.Errorf("unknown provider %q", id)
	}
	if model != "" && !validModelID(model) {
		return p.view(), fmt.Errorf("invalid model id %q", model)
	}
	if cheap != "" && !validModelID(cheap) {
		return p.view(), fmt.Errorf("invalid helper model id %q", cheap)
	}
	pc := p.read()
	ov := pc.Providers[id]
	if model != "" {
		ov.Model = model
	}
	if cheap != "" {
		ov.CheapModel = cheap
	}
	pc.Providers[id] = ov
	if err := p.write(pc); err != nil {
		return p.view(), err
	}
	return p.view(), nil
}

// setSecret writes/rotates the provider's backing secret through the secret store (which handles
// atomic write + restart of the consuming service), then returns the refreshed view.
func (p *providerStore) setSecret(ctx context.Context, id, value string) (providerView, []string, error) {
	d, ok := p.def(id)
	if !ok {
		return p.view(), nil, fmt.Errorf("unknown provider %q", id)
	}
	restarted, err := p.secrets.rotate(ctx, d.SecretName, value)
	if err != nil {
		return p.view(), restarted, err
	}
	return p.view(), restarted, nil
}

// modelIDRe bounds a model identifier to a sane charset (letters, digits, and . _ : - /), so a custom
// entry can't inject anything odd into provider.json or a downstream request.
var modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

func validModelID(s string) bool { return modelIDRe.MatchString(s) }
