// Package llm is a tiny client for the active provider's cheap "helper" model — single-shot
// classification / short-answer calls (route intent, resolve a repo from an issue, …). It is a leaf
// package (no internal imports) so both the control plane and the connectors can use it without a cycle.
//
// Route: it POSTs the Anthropic Messages API at BaseURL. On an API-key box that's the host gateway
// (which injects the real key, so a placeholder bearer is fine); on a subscription box it's Anthropic
// directly on the OAuth token. Callers resolve (BaseURL, Token, Model) from their side.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is a cheap-model caller. Zero value is unusable — use New.
type Client struct {
	BaseURL   string
	Token     string
	Model     string
	MaxTokens int
	HTTP      *http.Client
}

// New builds a client. maxTokens<=0 defaults to 16 (enough for a one-word class or a short slug).
func New(baseURL, token, model string) *Client {
	return &Client{
		BaseURL:   baseURL,
		Token:     token,
		Model:     model,
		MaxTokens: 16,
		HTTP:      &http.Client{Timeout: 25 * time.Second},
	}
}

// Classify sends system+user to the model and returns its lowercased text reply, or "" on any error
// (missing config, transport, non-2xx, decode). Callers apply their own fail-safe default on "".
func (c *Client) Classify(ctx context.Context, system, user string) string {
	if c.BaseURL == "" || c.Model == "" {
		return ""
	}
	maxTokens := c.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16
	}
	body, err := json.Marshal(map[string]any{
		"model":      c.Model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   []map[string]any{{"role": "user", "content": user}},
	})
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 25 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return ""
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	var t string
	for _, c := range out.Content {
		if c.Type == "text" {
			t += c.Text
		}
	}
	return strings.ToLower(t)
}
