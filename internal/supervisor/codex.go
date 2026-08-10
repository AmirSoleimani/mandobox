package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const codexBin = "codex"

// CodexRunner runs OpenAI's Codex CLI (`codex exec`) as the coding agent — the second AgentRunner,
// proving the harness seam (docs/configuration.md). It points Codex at the host LLM gateway's
// OpenAI-compatible endpoint (the gateway injects the real key host-side, so no key ever lives in the
// guest — I9), captures its output as the turn's reply, and leaves commit/push/PR to the supervisor's
// finalizeTurn — identical to Claude, since that machinery is harness-agnostic.
//
// UNVERIFIED against a live Codex CLI (needs an OpenAI key on the box). Two spots to confirm on the
// first real run (see docs/configuration.md "Enabling Codex"): the exact `codex exec` flags for
// non-interactive full-auto file access, and cost/token extraction (best-effort 0 here → the cost
// ceiling degrades to the wall-clock/TTL guardrail for Codex sessions until wired).
type CodexRunner struct{ bin string }

// NewCodexRunner returns a runner for the `codex` CLI.
func NewCodexRunner() *CodexRunner { return &CodexRunner{bin: codexBin} }

// Run executes `codex exec`, streaming each output line via onLine and returning the turn's reply
// (the tail of Codex's output). File changes it makes are committed/pushed/PR'd by the supervisor.
func (c *CodexRunner) Run(ctx context.Context, spec AgentSpec, onLine func([]byte)) (Result, error) {
	cmd := exec.CommandContext(ctx, c.bin, codexArgs(spec)...)
	cmd.Dir = spec.WorkDir
	cmd.Env = codexEnv(os.Environ(), spec)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("codex stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start codex: %w", err)
	}

	// Codex exec streams progress + a final message to stdout. Without a stable structured format we
	// republish every line to the log and keep the last lines as the turn's reply.
	var tail []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		onLine(line)
		if t := strings.TrimSpace(string(line)); t != "" {
			tail = append(tail, t)
			if len(tail) > 40 {
				tail = tail[1:]
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return Result{}, fmt.Errorf("codex exited: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return Result{Result: strings.Join(tail, "\n")}, nil
}

// codexArgs builds the `codex exec` invocation. Per-repo instructions (spec.SystemPrompt) are
// prepended to the prompt — a file-safe way to inject them without writing to the repo's AGENTS.md
// (which the supervisor would then commit). Resume is not yet wired: each turn is independent (a
// review round re-states context via the prompt), which is a documented v1 limitation for Codex.
func codexArgs(spec AgentSpec) []string {
	prompt := spec.Prompt
	if spec.SystemPrompt != "" {
		prompt = spec.SystemPrompt + "\n\n" + prompt
	}
	args := []string{"exec"}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	// The microVM is the sandbox, so run non-interactively with full file access. Exact flag TBD on
	// the first real run — codex has evolved its approval/sandbox flags.
	args = append(args, "--dangerously-bypass-approvals-and-sandbox", prompt)
	return args
}

// codexEnv points Codex at the host gateway's OpenAI-compatible endpoint. It strips any inherited LLM
// key, then sets OPENAI_API_KEY to the per-session token — which is safe (unlike Claude's dangerous
// ANTHROPIC_API_KEY): the gateway swaps this token for the real key host-side, so no real credential
// ever reaches the agent (I9).
func codexEnv(base []string, spec AgentSpec) []string {
	out := make([]string, 0, len(base)+6)
	for _, e := range base {
		if isAnthropicAPIKey(e) || isOpenAIAPIKey(e) {
			continue // never carry an inherited LLM key into the agent
		}
		out = append(out, e)
	}
	return append(out,
		"OPENAI_BASE_URL="+spec.BaseURL, // gateway reverse-proxy → LiteLLM (OpenAI-compatible)
		"OPENAI_API_KEY="+spec.AuthToken,
		"HTTPS_PROXY="+spec.BaseURL,
		"HTTP_PROXY="+spec.BaseURL,
		"NO_PROXY=169.254.169.254,localhost,127.0.0.1",
		"IS_SANDBOX=1",
	)
}

func isOpenAIAPIKey(env string) bool {
	k, _, ok := strings.Cut(env, "=")
	return ok && strings.EqualFold(k, "OPENAI_API_KEY")
}
