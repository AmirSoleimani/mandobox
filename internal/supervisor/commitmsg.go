package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// anthropicDirectURL is the real Anthropic endpoint the subscription path talks to directly (the
// egress gateway allowlists it). API-key providers use the host gateway base URL instead.
const anthropicDirectURL = "https://api.anthropic.com"

// cheapCommitModel is the small, fast model used only to write commit messages — a throwaway
// summarization job that doesn't warrant the session's main (expensive) model. It routes through
// the same host gateway; the wildcard LiteLLM config accepts any Anthropic model id.
const cheapCommitModel = "claude-haiku-4-5-20251001"

// maxCommitDiffBytes bounds how much diff we hand the model. A commit subject only needs the gist,
// and an unbounded diff would blow the request up on a large change.
const maxCommitDiffBytes = 12000

// GenerateCommitMessage asks a cheap model to write a real commit message from the diff and the
// task context, so history reads like what actually changed instead of a fixed placeholder. It
// talks to the same host gateway Claude Code uses (ANTHROPIC_BASE_URL + bearer auth token), so
// egress stays controlled. Best-effort: it returns "" on any error/timeout and the caller falls
// back to a static message — a commit must never be blocked by the model being unavailable.
func GenerateCommitMessage(ctx context.Context, baseURL, authToken, model, request, agentSummary, diffStat, diffPatch string) string {
	if baseURL == "" || authToken == "" {
		return ""
	}
	if model == "" {
		model = cheapCommitModel
	}
	if len(diffPatch) > maxCommitDiffBytes {
		diffPatch = diffPatch[:maxCommitDiffBytes] + "\n…(diff truncated)…"
	}

	var user strings.Builder
	if strings.TrimSpace(request) != "" {
		fmt.Fprintf(&user, "The request that led to this change:\n%s\n\n", strings.TrimSpace(request))
	}
	if strings.TrimSpace(agentSummary) != "" {
		fmt.Fprintf(&user, "What the engineer said they did:\n%s\n\n", truncateStr(strings.TrimSpace(agentSummary), 1500))
	}
	if strings.TrimSpace(diffStat) != "" {
		fmt.Fprintf(&user, "Files changed:\n%s\n\n", strings.TrimSpace(diffStat))
	}
	fmt.Fprintf(&user, "Diff:\n%s", diffPatch)

	reqBody := map[string]any{
		"model":      model,
		"max_tokens": 200,
		"system": "You write git commit messages. Given a diff and its context, output ONLY the " +
			"commit message — nothing else, no preamble, no backticks, no quotes. Format: a concise " +
			"imperative subject line under 72 characters that says what changed and why it matters, " +
			"optionally followed by a blank line and up to three short '- ' bullet points for detail. " +
			"Use a Conventional Commits prefix (feat:, fix:, docs:, refactor:, chore:, test:, perf:) " +
			"when one clearly fits. Describe the actual change, not the request.",
		"messages": []map[string]any{{"role": "user", "content": user.String()}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return ""
	}

	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+authToken) // mirrors ANTHROPIC_AUTH_TOKEN
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return ""
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	var msg strings.Builder
	for _, c := range out.Content {
		if c.Type == "text" {
			msg.WriteString(c.Text)
		}
	}
	return sanitizeCommitMessage(msg.String())
}

// sanitizeCommitMessage strips wrapping fences/quotes and common "here is…" preambles a model
// sometimes adds, and drops a message that came back empty.
func sanitizeCommitMessage(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "`")
	s = strings.TrimSpace(s)
	// A leading fenced block like ```\n<msg>\n``` — keep the inside.
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return ""
	}
	// Guard against an obviously non-message response (a refusal or explanation).
	first := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		first = s[:i]
	}
	if len(first) > 120 { // a real subject line is short; a paragraph means the model missed the format
		return ""
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
