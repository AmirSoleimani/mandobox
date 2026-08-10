package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const classifySystem = `You route messages sent to an AI coding assistant that is working on a pull ` +
	`request inside an isolated dev VM. Classify the user's message into exactly one word:
- ATTACH — the user wants to personally get into the dev environment to look at or edit the code by ` +
	`hand (e.g. "let me in", "can I get in", "I want to make the change myself", "give me a vscode ` +
	`link", "let me poke at it", "I'll edit it").
- DETACH — the user is finished in the environment and wants to close or leave it (e.g. "I'm done", ` +
	`"close it", "you can take it back now").
- MESSAGE — anything else: a coding request, a question, feedback, or discussion.
Reply with ONLY one word: ATTACH, DETACH, or MESSAGE.`

// ClassifyIntent decides whether a natural-language reply is a request to get into the VM (ATTACH),
// to leave it (DETACH), or a normal instruction (MESSAGE). It calls the cheap model through the same
// host gateway the guests use (which injects the key), so no extra secret is needed. Fail-safe:
// any error classifies as "message" so the reply still reaches the agent.
func (a *Activities) ClassifyIntent(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "message", nil
	}
	// Route through the active provider, same as the agent: subscription → Anthropic directly on the
	// OAuth token; API-key providers → the gateway (which injects the real key, so the placeholder
	// bearer is fine). Fail-safe: any error classifies as "message" so the reply still reaches the agent.
	baseURL, token, model := a.resolveProvider().helperLLM(a.GatewayURL)
	if baseURL == "" || model == "" {
		return "message", nil
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 5,
		"system":     classifySystem,
		"messages":   []map[string]any{{"role": "user", "content": message}},
	})
	if err != nil {
		return "message", nil
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "message", nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return "message", nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "message", nil
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "message", nil
	}
	var t string
	for _, c := range out.Content {
		if c.Type == "text" {
			t += c.Text
		}
	}
	t = strings.ToLower(t)
	switch {
	case strings.Contains(t, "attach"):
		return "attach", nil
	case strings.Contains(t, "detach"):
		return "detach", nil
	default:
		return "message", nil
	}
}
