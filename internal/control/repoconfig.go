package control

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Configuration model (see docs/configuration.md). Two files feed one resolution:
//   - BoxConfig: the operator's /etc/fleet/mandobox.yml — defaults everyone inherits + guardrails.
//   - RepoConfig: a repo's committed .mandobox.yml — a repo author's requests, clamped to the box.
// resolveConfig folds them (plus per-task overrides on WorkflowInput) into a ResolvedConfig the
// workflow applies, along with human-readable warnings for anything it clamped or dropped.

// Duration unmarshals a YAML string like "15m"/"2h" into a time.Duration.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(strings.TrimSpace(n.Value))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	*d = Duration(v)
	return nil
}

// ---- operator (box) config ----

type BoxConfig struct {
	Defaults BoxDefaults `yaml:"defaults"`
	Limits   BoxLimits   `yaml:"limits"`
}

type BoxDefaults struct {
	Agent     string       `yaml:"agent"`
	Model     string       `yaml:"model"`
	Resources string       `yaml:"resources"` // profile name
	KeepAlive Duration     `yaml:"keep_alive"`
	Review    ReviewConfig `yaml:"review"`
	// Instructions is the box-wide default system-prompt addition every session inherits unless its
	// repo's .mandobox.yml sets its own. Usually managed via the dashboard (a plain-text file the
	// ResolveConfig activity loads into this field), but may also be set inline here.
	Instructions string `yaml:"instructions"`
}

type BoxLimits struct {
	Resources      ResourceLimits `yaml:"resources"`
	CostCeilingUSD float64        `yaml:"cost_ceiling_usd"`
	HardTTL        Duration       `yaml:"hard_ttl"`
	Concurrency    int            `yaml:"concurrency"`
	AgentsAllowed  []string       `yaml:"agents_allowed"`
	ModelsAllowed  []string       `yaml:"models_allowed"`
}

type ResourceLimits struct {
	Profiles   map[string]ResourceProfile `yaml:"profiles"`
	MaxProfile string                     `yaml:"max_profile"`
}

type ResourceProfile struct {
	VCPUs int `yaml:"vcpus"`
	Mem   int `yaml:"mem"`  // MiB
	Disk  int `yaml:"disk"` // MiB (reserved — workspace sizing is not per-session yet)
}

type ReviewConfig struct {
	MaxRounds int  `yaml:"max_rounds"`
	AutoFixCI bool `yaml:"auto_fix_ci"`
}

// ---- repo (.mandobox.yml) config ----
//
// Optional fields are pointers so we can tell "unset" from a zero value. Operator-only keys are
// captured only to warn that they're ignored in a repo file.
type RepoConfig struct {
	Agent        string      `yaml:"agent"`
	Model        string      `yaml:"model"`
	Resources    string      `yaml:"resources"`
	KeepAlive    *Duration   `yaml:"keep_alive"`
	Review       *RepoReview `yaml:"review"`
	Instructions string      `yaml:"instructions"` // applied to the guest in a later slice

	// operator-only — present only so we can warn they were ignored:
	CostCeilingUSD *float64  `yaml:"cost_ceiling_usd"`
	Concurrency    *int      `yaml:"concurrency"`
	Egress         yaml.Node `yaml:"egress"`
	Secrets        yaml.Node `yaml:"secrets"`
}

type RepoReview struct {
	MaxRounds *int  `yaml:"max_rounds"`
	AutoFixCI *bool `yaml:"auto_fix_ci"`
}

// ResolvedConfig is the effective, clamped configuration the workflow applies. It is an activity
// return value, so it holds plain fields plus the Policy the workflow drives.
type ResolvedConfig struct {
	VCPUs        int      `json:"vcpus"`
	MemMiB       int      `json:"mem_mib"`
	Model        string   `json:"model"`
	Agent        string   `json:"agent"`
	Instructions string   `json:"instructions"`
	Policy       Policy   `json:"policy"`
	Warnings     []string `json:"warnings"`
}

// DefaultBoxConfig reproduces today's baked-in behavior, so a box with no mandobox.yml and a repo
// with no .mandobox.yml resolve to exactly what mandobox does now.
func DefaultBoxConfig() BoxConfig {
	return BoxConfig{
		Defaults: BoxDefaults{
			Agent:     "claude",
			Model:     "claude-sonnet-5",
			Resources: "medium",
			KeepAlive: Duration(15 * time.Minute),
			Review:    ReviewConfig{MaxRounds: 5},
		},
		Limits: BoxLimits{
			Resources: ResourceLimits{
				Profiles: map[string]ResourceProfile{
					"small":  {VCPUs: 1, Mem: 2048, Disk: 4096},
					"medium": {VCPUs: 2, Mem: 4096, Disk: 8192},
					"large":  {VCPUs: 4, Mem: 8192, Disk: 16384},
				},
				MaxProfile: "large",
			},
			CostCeilingUSD: 15,
			HardTTL:        Duration(14 * 24 * time.Hour),
			Concurrency:    8,
			AgentsAllowed:  []string{"claude"},
			ModelsAllowed:  []string{"claude-sonnet-5", "claude-haiku-4-5-20251001"},
		},
	}
}

// LoadBoxConfig reads the operator config, layering it over DefaultBoxConfig so a partial file still
// yields a complete config. A missing file is fine (pure defaults).
func LoadBoxConfig(path string) (BoxConfig, error) {
	cfg := DefaultBoxConfig()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return DefaultBoxConfig(), fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// ParseRepoConfig decodes a .mandobox.yml. Empty input yields a zero RepoConfig (all defaults).
func ParseRepoConfig(b []byte) (RepoConfig, error) {
	var rc RepoConfig
	if len(strings.TrimSpace(string(b))) == 0 {
		return rc, nil
	}
	if err := yaml.Unmarshal(b, &rc); err != nil {
		return RepoConfig{}, fmt.Errorf("parse .mandobox.yml: %w", err)
	}
	return rc, nil
}

// profileRank orders resource profiles by size (vCPUs then memory) so "clamp to max_profile" has a
// meaning. A profile the box doesn't define ranks -1 (unknown).
func profileRank(box BoxConfig, name string) int {
	names := make([]string, 0, len(box.Limits.Resources.Profiles))
	for n := range box.Limits.Resources.Profiles {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := box.Limits.Resources.Profiles[names[i]], box.Limits.Resources.Profiles[names[j]]
		if a.VCPUs != b.VCPUs {
			return a.VCPUs < b.VCPUs
		}
		return a.Mem < b.Mem
	})
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

// resolveConfig folds box defaults, the repo file, and per-task overrides (non-zero WorkflowInput
// fields) into the effective config, clamping guarded settings to the box limits and collecting a
// warning for every adjustment. Pure and deterministic.
func resolveConfig(box BoxConfig, repo RepoConfig, in WorkflowInput) ResolvedConfig {
	var warn []string
	add := func(format string, a ...any) { warn = append(warn, fmt.Sprintf(format, a...)) }

	// ---- resources (named profile, clamped to max_profile) ----
	profName := firstNonEmpty(repo.Resources, box.Defaults.Resources)
	if _, ok := box.Limits.Resources.Profiles[profName]; !ok {
		add("resource profile %q isn't defined — using %q.", profName, box.Defaults.Resources)
		profName = box.Defaults.Resources
	}
	if maxName := box.Limits.Resources.MaxProfile; maxName != "" &&
		profileRank(box, profName) > profileRank(box, maxName) {
		add("resource profile %q exceeds the box max — using %q.", profName, maxName)
		profName = maxName
	}
	prof := box.Limits.Resources.Profiles[profName]
	vcpus, mem := prof.VCPUs, prof.Mem
	if in.VCPUs != 0 { // explicit per-task override, still clamped to the chosen profile
		vcpus = clampInt(in.VCPUs, 1, prof.VCPUs)
	}
	if in.MemMiB != 0 {
		mem = clampInt(in.MemMiB, 512, prof.Mem)
	}

	// ---- model (task > repo > box; must be in the allowlist) ----
	model := firstNonEmpty(in.Model, repo.Model, box.Defaults.Model)
	if !allowed(box.Limits.ModelsAllowed, model) {
		add("model %q isn't in models_allowed — using %q.", model, box.Defaults.Model)
		model = box.Defaults.Model
	}

	// ---- agent harness (repo > box; must be in the allowlist) ----
	agent := firstNonEmpty(repo.Agent, box.Defaults.Agent)
	if !allowed(box.Limits.AgentsAllowed, agent) {
		add("agent %q isn't in agents_allowed — using %q.", agent, box.Defaults.Agent)
		agent = box.Defaults.Agent
	}

	// ---- policy ----
	pol := Policy{
		MaxReviewRounds: box.Defaults.Review.MaxRounds,
		AutoFixCI:       box.Defaults.Review.AutoFixCI,
		CostCeilingUSD:  box.Limits.CostCeilingUSD, // operator-only ceiling
		HardTTL:         time.Duration(box.Limits.HardTTL),
		KeepAlive:       time.Duration(box.Defaults.KeepAlive),
	}
	if repo.Review != nil {
		if repo.Review.MaxRounds != nil {
			pol.MaxReviewRounds = *repo.Review.MaxRounds
		}
		if repo.Review.AutoFixCI != nil {
			pol.AutoFixCI = *repo.Review.AutoFixCI
		}
	}
	if repo.KeepAlive != nil {
		ka := time.Duration(*repo.KeepAlive)
		if pol.HardTTL > 0 && ka > pol.HardTTL {
			add("keep_alive %s exceeds hard_ttl %s — clamping.", ka, pol.HardTTL)
			ka = pol.HardTTL
		}
		pol.KeepAlive = ka
	}

	// ---- operator-only keys in a repo file: dropped + warned ----
	if repo.CostCeilingUSD != nil {
		add("cost_ceiling_usd is operator-only — ignoring (using $%.2f).", pol.CostCeilingUSD)
	}
	if repo.Concurrency != nil {
		add("concurrency is operator-only — ignoring.")
	}
	if !repo.Egress.IsZero() {
		add("egress is operator-only — ignoring.")
	}
	if !repo.Secrets.IsZero() {
		add("secrets can't be set from a repo — ignoring.")
	}

	return ResolvedConfig{
		VCPUs: vcpus, MemMiB: mem, Model: model, Agent: agent,
		// Repo instructions override the box-wide default (consistent with every other field: a
		// repo's .mandobox.yml wins over the box default).
		Instructions: firstNonEmpty(repo.Instructions, box.Defaults.Instructions),
		Policy:       pol, Warnings: warn,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func allowed(list []string, v string) bool {
	if len(list) == 0 {
		return true // no allowlist configured → allow anything
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if hi > 0 && v > hi {
		return hi
	}
	return v
}
