package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const claudeBin = "claude"

// AgentSpec is one Claude Code invocation.
type AgentSpec struct {
	WorkDir         string
	Prompt          string
	Model           string
	Resume          bool
	ClaudeSessionID string // Claude's own session UUID, captured from the initial run
	BaseURL         string // host LLM gateway
	AuthToken       string // per-session bearer token
}

// AgentRunner runs the coding agent. It is an interface so another harness could slot in
// (PLAN §1 non-goal, but the seam is kept) and so orchestration is testable with a fake.
type AgentRunner interface {
	Run(ctx context.Context, spec AgentSpec, onLine func([]byte)) (Result, error)
}

// ClaudeRunner runs Claude Code headless, streaming stream-json (§8.2).
type ClaudeRunner struct{ bin string }

// NewClaudeRunner returns a runner for the pinned `claude` CLI.
func NewClaudeRunner() *ClaudeRunner { return &ClaudeRunner{bin: claudeBin} }

// Run executes Claude Code, republishing each stream-json line via onLine and returning the
// terminal Result (usage/cost).
func (c *ClaudeRunner) Run(ctx context.Context, spec AgentSpec, onLine func([]byte)) (Result, error) {
	env, err := agentEnv(os.Environ(), spec)
	if err != nil {
		return Result{}, err
	}
	cmd := exec.CommandContext(ctx, c.bin, claudeArgs(spec)...)
	cmd.Dir = spec.WorkDir
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("claude stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start claude: %w", err)
	}
	res, perr := ParseStream(stdout, onLine)
	werr := cmd.Wait()
	if werr != nil {
		return res, fmt.Errorf("claude exited: %w: %s", werr, strings.TrimSpace(stderr.String()))
	}
	if perr != nil {
		return Result{}, perr
	}
	return res, nil
}

// claudeArgs builds the CLI arguments (§8.2). Resume reuses Claude's own session so the
// transcript on the workspace volume continues (§8.1).
func claudeArgs(spec AgentSpec) []string {
	args := []string{
		"-p", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Resume && spec.ClaudeSessionID != "" {
		args = append(args, "--resume", spec.ClaudeSessionID)
	}
	return args
}

// agentEnv builds the agent's environment: it points Claude Code at the host gateway and
// strips ANTHROPIC_API_KEY, which must never be set in a guest — if it were, Claude Code
// would prefer it over the gateway token and bypass the proxy entirely (invariant I9, §10).
func agentEnv(base []string, spec AgentSpec) ([]string, error) {
	out := make([]string, 0, len(base)+5)
	for _, e := range base {
		if isAnthropicAPIKey(e) {
			continue // I9: strip any inherited key
		}
		out = append(out, e)
	}
	out = append(out,
		"ANTHROPIC_BASE_URL="+spec.BaseURL,
		"ANTHROPIC_AUTH_TOKEN="+spec.AuthToken,
		"HTTPS_PROXY="+spec.BaseURL,
		"HTTP_PROXY="+spec.BaseURL,
		"NO_PROXY=169.254.169.254,localhost,127.0.0.1",
	)
	if slices.ContainsFunc(out, isAnthropicAPIKey) {
		return nil, errors.New("I9 violation: ANTHROPIC_API_KEY present in agent environment")
	}
	return out, nil
}

func isAnthropicAPIKey(env string) bool {
	k, _, ok := strings.Cut(env, "=")
	return ok && strings.EqualFold(k, "ANTHROPIC_API_KEY")
}
