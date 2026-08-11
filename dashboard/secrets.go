package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// secrets.go manages the box's secrets by STATUS and ROTATION only — it never returns or logs a
// secret value. The UI shows presence, permissions, mtime, and a short sha256 fingerprint (enough
// to confirm a rotation changed the value, useless for recovering it). Rotation writes the new
// value with 0600 perms and restarts the consuming service(s).
//
// Source-of-truth caveat surfaced in the UI: on a single box the operator's Ansible `secrets/` dir
// (on the controller) is what a re-deploy re-applies. A rotation here takes effect immediately but
// is reverted by the next `ansible-playbook deploy.yml` unless the controller copy is updated too.

// secretTarget is one file the secret's value is written to. For "env" kind, Key is the VAR name
// on a KEY=value line; for "file" kind, the whole file IS the value (Key empty).
type secretTarget struct {
	Path string
	Key  string // env var name, or "" for whole-file secrets
}

// secretDef declares a managed secret. Restart lists candidate unit names per logical service —
// the first that exists on the box wins (the box may run pre-rename `fleet-*` units).
type secretDef struct {
	Name     string
	Label    string
	Desc     string // what the secret is used for (shown in the UI)
	Kind     string // "env" | "file"
	Targets  []secretTarget
	Restart  [][]string // each inner slice = candidate unit names for one service
	Hint     string
	Editable bool
}

// managedSecrets is the registry. Paths match what the Ansible roles install (verified on the box).
func managedSecrets() []secretDef {
	return []secretDef{
		{
			Name: "anthropic-api-key", Label: "Anthropic API key", Kind: "env",
			Desc:    "The real Claude key. Held host-side in LiteLLM and injected into upstream calls by the egress gateway — it never reaches a guest VM. Powers every Claude session.",
			Targets: []secretTarget{{Path: "/etc/litellm/litellm.env", Key: "ANTHROPIC_API_KEY"}},
			Restart: [][]string{{"litellm"}}, Hint: "sk-ant-…", Editable: true,
		},
		{
			Name: "openai-api-key", Label: "OpenAI API key (Codex)", Kind: "env",
			Desc:    "Optional. Enables the OpenAI/Codex harness via LiteLLM. Only needed if you add Codex to agents_allowed; leave unset for a Claude-only box.",
			Targets: []secretTarget{{Path: "/etc/litellm/litellm.env", Key: "OPENAI_API_KEY"}},
			Restart: [][]string{{"litellm"}}, Hint: "sk-…", Editable: true,
		},
		{
			// Lives in two places: LiteLLM's env and the gateway's upstream.key (the header the
			// gateway injects toward LiteLLM). Rotating writes both and restarts both.
			Name: "litellm-master-key", Label: "LiteLLM master key", Kind: "env",
			Desc: "The internal key the egress gateway presents to LiteLLM to authorize a guest's LLM traffic. Not a provider key — an internal shared secret between the gateway and LiteLLM.",
			Targets: []secretTarget{
				{Path: "/etc/litellm/litellm.env", Key: "LITELLM_MASTER_KEY"},
				{Path: "/etc/fleet/gateway/upstream.key", Key: ""},
			},
			Restart: [][]string{{"litellm"}, {"mando-gateway", "fleet-gateway"}}, Hint: "sk-…", Editable: true,
		},
		{
			Name: "slack-bot-token", Label: "Slack bot token", Kind: "env",
			Desc:    "Slack bot OAuth token (xoxb-). Lets the worker post session updates and the connector host read commands in your Slack workspace.",
			Targets: []secretTarget{{Path: "/etc/fleet/slack.env", Key: "SLACK_BOT_TOKEN"}},
			Restart: [][]string{{"mando-worker", "fleet-worker"}, {"mando-connectors"}}, Hint: "xoxb-…", Editable: true,
		},
		{
			Name: "slack-app-token", Label: "Slack app token", Kind: "env",
			Desc:    "Slack app-level token (xapp-) for Socket Mode. Lets the gateway receive Slack events without a public webhook.",
			Targets: []secretTarget{{Path: "/etc/fleet/slack.env", Key: "SLACK_APP_TOKEN"}},
			Restart: [][]string{{"mando-worker", "fleet-worker"}, {"mando-connectors"}}, Hint: "xapp-…", Editable: true,
		},
		{
			Name: "telegram-bot-token", Label: "Telegram bot token", Kind: "env",
			Desc:    "Telegram Bot API token (from @BotFather). Lets the worker post session updates and the connector host receive /mando commands and chat replies. Optional.",
			Targets: []secretTarget{{Path: "/etc/fleet/telegram.env", Key: "TELEGRAM_BOT_TOKEN"}},
			Restart: [][]string{{"mando-worker", "fleet-worker"}, {"mando-connectors"}}, Hint: "123456789:ABC-…", Editable: true,
		},
		{
			Name: "webhook-secret", Label: "GitHub webhook secret", Kind: "env",
			Desc:    "HMAC secret that signs GitHub webhook deliveries. webhook-rx verifies every payload against it, so only GitHub-signed events (PR reviews, comments, CI) drive a session.",
			Targets: []secretTarget{{Path: "/etc/fleet/webhook-secret.env", Key: "GITHUB_WEBHOOK_SECRET"}},
			Restart: [][]string{{"webhook-rx"}}, Hint: "hex string", Editable: true,
		},
		{
			Name: "github-app-key", Label: "GitHub App private key", Kind: "file",
			Desc:    "The GitHub App's private key (PEM). The worker mints short-lived installation tokens with it to clone repos, push branches, and open/update PRs on your behalf.",
			Targets: []secretTarget{{Path: "/etc/fleet/github-app.pem"}},
			Restart: [][]string{{"mando-worker", "fleet-worker"}}, Hint: "full PEM (-----BEGIN…)", Editable: true,
		},
		{
			Name: "claude-oauth-token", Label: "Claude subscription token", Kind: "file",
			Desc:    "Optional, single-user only. A long-lived `claude setup-token` (sk-ant-oat01-…) that runs the agent on your Claude plan instead of an API key. Select Claude Subscription in Config → Model. See docs/subscription-auth.md.",
			Targets: []secretTarget{{Path: "/etc/fleet/claude-oauth-token"}},
			Restart: [][]string{}, // none — the worker re-reads it per launch
			Hint:    "sk-ant-oat01-…", Editable: true,
		},
		{
			Name: "vscode-tunnel-token", Label: "VS Code tunnel token", Kind: "file",
			Desc:    "A pre-authenticated `code tunnel` token, injected into guests so a human can attach VS Code to a live VM without doing the device login each time.",
			Targets: []secretTarget{{Path: "/etc/fleet/vscode-tunnel-token.json"}},
			Restart: [][]string{{"mando-worker", "fleet-worker"}}, Hint: "token.json contents", Editable: true,
		},
	}
}

type secretStore struct {
	defs     []secretDef
	auditLog string // /var/lib/fleet/secret-rotations.log

	mu        sync.Mutex
	unitCache map[string]string // candidates-key → resolved loaded unit (this box's real names)
}

func newSecretStore(auditLog string) *secretStore {
	return &secretStore{defs: managedSecrets(), auditLog: auditLog, unitCache: map[string]string{}}
}

// resolvedRestarts maps each service's candidate list to the unit actually loaded on this box, so
// the UI shows real names (e.g. fleet-worker, not the canonical mando-worker). Cached per process.
func (s *secretStore) resolvedRestarts(restart [][]string) []string {
	out := make([]string, 0, len(restart))
	for _, candidates := range restart {
		out = append(out, s.resolveUnit(candidates))
	}
	return out
}

func (s *secretStore) resolveUnit(candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	key := strings.Join(candidates, "|")
	s.mu.Lock()
	if u, ok := s.unitCache[key]; ok {
		s.mu.Unlock()
		return u
	}
	s.mu.Unlock()

	resolved := candidates[0] // fallback to the canonical name if none is loaded
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, u := range candidates {
		if unitLoaded(ctx, u) {
			resolved = u
			break
		}
	}
	s.mu.Lock()
	s.unitCache[key] = resolved
	s.mu.Unlock()
	return resolved
}

type secretView struct {
	Name        string   `json:"name"`
	Label       string   `json:"label"`
	Desc        string   `json:"desc,omitempty"`
	Kind        string   `json:"kind"`
	Path        string   `json:"path"`
	Present     bool     `json:"present"`
	Fingerprint string   `json:"fingerprint,omitempty"` // short sha256 of the value; never the value
	Mode        string   `json:"mode,omitempty"`
	Modified    string   `json:"modified,omitempty"`
	Restarts    []string `json:"restarts"`
	Hint        string   `json:"hint,omitempty"`
	Editable    bool     `json:"editable"`
	LastRotated string   `json:"last_rotated,omitempty"` // from the dashboard's own audit log
}

func (s *secretStore) view() []secretView {
	rotations := s.lastRotations()
	out := make([]secretView, 0, len(s.defs))
	for _, d := range s.defs {
		t := d.Targets[0]
		v := secretView{
			Name: d.Name, Label: d.Label, Desc: d.Desc, Kind: d.Kind, Path: t.Path,
			Restarts: s.resolvedRestarts(d.Restart), Hint: d.Hint, Editable: d.Editable,
			LastRotated: rotations[d.Name],
		}
		if fi, err := os.Stat(t.Path); err == nil {
			v.Mode = fi.Mode().Perm().String()
			v.Modified = fi.ModTime().UTC().Format(time.RFC3339)
			if val, ok := readSecretValue(t); ok && val != "" {
				v.Present = true
				v.Fingerprint = fingerprint(val)
			}
		}
		out = append(out, v)
	}
	return out
}

// oneView returns the view for a single secret by name (used by the provider cards to show whether
// a provider's backing secret is installed).
func (s *secretStore) oneView(name string) (secretView, bool) {
	for _, v := range s.view() {
		if v.Name == name {
			return v, true
		}
	}
	return secretView{}, false
}

// readSecretValue returns the value a target holds (env var line value, or whole file), used only
// to fingerprint it. Never returned to a client.
func readSecretValue(t secretTarget) (string, bool) {
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return "", false
	}
	if t.Key == "" {
		return strings.TrimSpace(string(b)), true
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, val, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == t.Key {
			return strings.TrimSpace(val), true
		}
	}
	return "", false
}

func fingerprint(val string) string {
	sum := sha256.Sum256([]byte(val))
	return hex.EncodeToString(sum[:])[:12]
}

// rotate writes newVal to every target and restarts the consuming services. It validates non-empty
// input, preserves other keys in an env file, and writes 0600. Returns the services it restarted.
func (s *secretStore) rotate(ctx context.Context, name, newVal string) ([]string, error) {
	def, ok := s.def(name)
	if !ok {
		return nil, fmt.Errorf("unknown secret %q", name)
	}
	if !def.Editable {
		return nil, fmt.Errorf("secret %q is not editable", name)
	}
	newVal = strings.TrimRight(newVal, "\r\n")
	if strings.TrimSpace(newVal) == "" {
		return nil, fmt.Errorf("new value is empty")
	}

	for _, t := range def.Targets {
		if err := writeSecretTarget(t, newVal); err != nil {
			return nil, err
		}
	}

	restarted := make([]string, 0, len(def.Restart))
	var restartErrs []string
	for _, candidates := range def.Restart {
		unit, err := restartFirst(ctx, candidates)
		if err != nil {
			restartErrs = append(restartErrs, err.Error())
			continue
		}
		restarted = append(restarted, unit)
	}

	s.audit(name, fingerprint(newVal), restarted)
	if len(restartErrs) > 0 {
		// The value is written; only the restart failed — report it so the operator can act.
		return restarted, fmt.Errorf("rotated, but restart failed: %s", strings.Join(restartErrs, "; "))
	}
	return restarted, nil
}

// writeSecretTarget replaces one env key (preserving the rest of the file) or the whole file,
// atomically, with 0600 perms.
func writeSecretTarget(t secretTarget, val string) error {
	var content string
	if t.Key == "" {
		content = val + "\n"
	} else {
		cur, _ := os.ReadFile(t.Path) // absent file → start fresh
		content = upsertEnvLine(string(cur), t.Key, val)
	}
	tmp := t.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", t.Path, err)
	}
	if err := os.Rename(tmp, t.Path); err != nil {
		return fmt.Errorf("replace %s: %w", t.Path, err)
	}
	return nil
}

// upsertEnvLine sets KEY=val in an env file body, replacing an existing line or appending one, and
// leaving every other line untouched.
func upsertEnvLine(body, key, val string) string {
	lines := strings.Split(body, "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == key {
			lines[i] = key + "=" + val
			replaced = true
			break
		}
	}
	if !replaced {
		// Drop a trailing empty element so we don't accumulate blank lines, then append.
		for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, key+"="+val)
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// restartFirst restarts the first candidate unit that exists (LoadState=loaded), so it works
// whether the box runs `mando-*` or the older `fleet-*` unit names.
func restartFirst(ctx context.Context, candidates []string) (string, error) {
	for _, unit := range candidates {
		if !unitLoaded(ctx, unit) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := exec.CommandContext(cctx, "systemctl", "restart", unit).Run()
		cancel()
		if err != nil {
			return "", fmt.Errorf("restart %s: %w", unit, err)
		}
		return unit, nil
	}
	return "", fmt.Errorf("no unit found among %s", strings.Join(candidates, ", "))
}

func unitLoaded(ctx context.Context, unit string) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "systemctl", "show", "-p", "LoadState", "--value", unit).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "loaded"
}

// audit appends a rotation record (name, ts, new fingerprint, restarted units) — never the value.
func (s *secretStore) audit(name, fp string, restarted []string) {
	ts := nowFn().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("%s\tsecret=%s\tfingerprint=%s\trestarted=%s\n", ts, name, fp, strings.Join(restarted, ","))
	f, err := os.OpenFile(s.auditLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(line)
}

// lastRotations reads the audit log and returns the newest rotation timestamp per secret.
func (s *secretStore) lastRotations() map[string]string {
	out := map[string]string{}
	f, err := os.Open(s.auditLog)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ts, name string
		for f := range strings.SplitSeq(line, "\t") {
			if after, ok := strings.CutPrefix(f, "secret="); ok {
				name = after
			} else if ts == "" {
				ts = f
			}
		}
		if name != "" {
			out[name] = ts // later lines overwrite earlier → newest wins
		}
	}
	return out
}

func (s *secretStore) def(name string) (secretDef, bool) {
	for _, d := range s.defs {
		if d.Name == name {
			return d, true
		}
	}
	return secretDef{}, false
}

// restartLabels flattens candidate lists to their first name for display ("what will restart").
func restartLabels(restart [][]string) []string {
	out := make([]string, 0, len(restart))
	for _, c := range restart {
		if len(c) > 0 {
			out = append(out, c[0])
		}
	}
	return out
}
