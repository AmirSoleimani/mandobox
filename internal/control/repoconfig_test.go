package control

import (
	"strings"
	"testing"
	"time"
)

func hasWarn(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestResolveConfig(t *testing.T) {
	box := DefaultBoxConfig()

	t.Run("empty repo → box defaults", func(t *testing.T) {
		r := resolveConfig(box, RepoConfig{}, WorkflowInput{})
		if r.VCPUs != 2 || r.MemMiB != 4096 {
			t.Errorf("resources = %d/%d, want medium 2/4096", r.VCPUs, r.MemMiB)
		}
		if r.Model != "claude-sonnet-5" || r.Agent != "claude" {
			t.Errorf("model/agent = %q/%q", r.Model, r.Agent)
		}
		if r.Policy.MaxReviewRounds != 5 || r.Policy.CostCeilingUSD != 15 {
			t.Errorf("policy = %+v", r.Policy)
		}
		if len(r.Warnings) != 0 {
			t.Errorf("unexpected warnings: %v", r.Warnings)
		}
	})

	t.Run("repo picks large", func(t *testing.T) {
		r := resolveConfig(box, RepoConfig{Resources: "large"}, WorkflowInput{})
		if r.VCPUs != 4 || r.MemMiB != 8192 {
			t.Errorf("resources = %d/%d, want large 4/8192", r.VCPUs, r.MemMiB)
		}
	})

	t.Run("box default instructions apply when repo sets none", func(t *testing.T) {
		b := box
		b.Defaults.Instructions = "Always follow the house style."
		r := resolveConfig(b, RepoConfig{}, WorkflowInput{})
		if r.Instructions != "Always follow the house style." {
			t.Errorf("instructions = %q, want box default", r.Instructions)
		}
	})

	t.Run("repo instructions override the box default", func(t *testing.T) {
		b := box
		b.Defaults.Instructions = "box-wide"
		r := resolveConfig(b, RepoConfig{Instructions: "repo-specific"}, WorkflowInput{})
		if r.Instructions != "repo-specific" {
			t.Errorf("instructions = %q, want repo override", r.Instructions)
		}
	})

	t.Run("over-max profile is clamped + warned", func(t *testing.T) {
		b := box
		b.Limits.Resources.MaxProfile = "medium"
		r := resolveConfig(b, RepoConfig{Resources: "large"}, WorkflowInput{})
		if r.VCPUs != 2 {
			t.Errorf("expected clamp to medium, got %d vcpus", r.VCPUs)
		}
		if !hasWarn(r.Warnings, "exceeds the box max") {
			t.Errorf("expected clamp warning, got %v", r.Warnings)
		}
	})

	t.Run("model not in allowlist falls back + warns", func(t *testing.T) {
		r := resolveConfig(box, RepoConfig{Model: "gpt-5-codex"}, WorkflowInput{})
		if r.Model != "claude-sonnet-5" {
			t.Errorf("expected fallback model, got %q", r.Model)
		}
		if !hasWarn(r.Warnings, "models_allowed") {
			t.Errorf("expected allowlist warning, got %v", r.Warnings)
		}
	})

	t.Run("keep_alive over hard_ttl is clamped", func(t *testing.T) {
		b := box
		b.Limits.HardTTL = Duration(time.Hour)
		ka := Duration(2 * time.Hour)
		r := resolveConfig(b, RepoConfig{KeepAlive: &ka}, WorkflowInput{})
		if r.Policy.KeepAlive != time.Hour {
			t.Errorf("keep_alive = %s, want clamp to 1h", r.Policy.KeepAlive)
		}
		if !hasWarn(r.Warnings, "keep_alive") {
			t.Errorf("expected keep_alive warning, got %v", r.Warnings)
		}
	})

	t.Run("operator-only key in repo is dropped + warned", func(t *testing.T) {
		c := 999.0
		r := resolveConfig(box, RepoConfig{CostCeilingUSD: &c}, WorkflowInput{})
		if r.Policy.CostCeilingUSD != 15 {
			t.Errorf("cost ceiling = %v, want box 15", r.Policy.CostCeilingUSD)
		}
		if !hasWarn(r.Warnings, "operator-only") {
			t.Errorf("expected operator-only warning, got %v", r.Warnings)
		}
	})

	t.Run("task override beats repo and box", func(t *testing.T) {
		r := resolveConfig(box, RepoConfig{Model: "claude-sonnet-5"},
			WorkflowInput{Model: "claude-haiku-4-5-20251001"})
		if r.Model != "claude-haiku-4-5-20251001" {
			t.Errorf("task model should win, got %q", r.Model)
		}
	})
}

func TestParseRepoConfig(t *testing.T) {
	rc, err := ParseRepoConfig([]byte("agent: codex\nmodel: gpt-5-codex\nresources: large\nkeep_alive: 2h\nreview:\n  max_rounds: 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if rc.Agent != "codex" || rc.Resources != "large" {
		t.Errorf("parsed %+v", rc)
	}
	if rc.KeepAlive == nil || time.Duration(*rc.KeepAlive) != 2*time.Hour {
		t.Errorf("keep_alive not parsed: %v", rc.KeepAlive)
	}
	if rc.Review == nil || rc.Review.MaxRounds == nil || *rc.Review.MaxRounds != 3 {
		t.Errorf("review not parsed: %+v", rc.Review)
	}
}
