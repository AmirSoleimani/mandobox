package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Slack outbound: one thread per session. PostSlack starts the thread and replies
// to it; UpdateSlack edits a posted message. These run as Temporal activities. Inbound (the
// /fleet command, thread replies, needs_input answers) is handled by cmd/slack-gateway over
// Socket Mode. If no bot token is configured the activities are graceful no-ops, so a task
// dispatched from the CLI still runs without Slack.

// PostSlackParams posts a message; ThreadTS empty starts a new thread (the root message).
type PostSlackParams struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// PostSlackResult carries the message timestamp — the root message's TS is the thread key.
type PostSlackResult struct {
	TS      string `json:"ts"`
	Channel string `json:"channel"`
}

// UpdateSlackParams edits a previously posted message (chat.update).
type UpdateSlackParams struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
	Text    string `json:"text"`
}

func (a *Activities) slackHTTP() *http.Client {
	if a.slackClient != nil {
		return a.slackClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// PostSlack posts a message and returns its TS. No-op (empty result) when Slack is unconfigured.
func (a *Activities) PostSlack(ctx context.Context, p PostSlackParams) (PostSlackResult, error) {
	if a.SlackBotToken == "" {
		return PostSlackResult{}, nil
	}
	if p.Channel == "" {
		p.Channel = a.SlackChannel
	}
	// No link previews — the thread carries a lot of URLs (PRs, tunnel links) and unfurled cards are
	// noise the operator didn't ask for.
	body := map[string]any{"channel": p.Channel, "text": p.Text, "unfurl_links": false, "unfurl_media": false}
	if p.ThreadTS != "" {
		body["thread_ts"] = p.ThreadTS
	}
	var out struct {
		OK      bool   `json:"ok"`
		TS      string `json:"ts"`
		Channel string `json:"channel"`
		Error   string `json:"error"`
	}
	if err := a.slackCall(ctx, "chat.postMessage", body, &out); err != nil {
		return PostSlackResult{}, err
	}
	if !out.OK {
		return PostSlackResult{}, fmt.Errorf("slack chat.postMessage: %s", out.Error)
	}
	return PostSlackResult{TS: out.TS, Channel: out.Channel}, nil
}

// UpdateSlack edits a message in place. No-op when Slack is unconfigured or TS is empty.
func (a *Activities) UpdateSlack(ctx context.Context, p UpdateSlackParams) error {
	if a.SlackBotToken == "" || p.TS == "" {
		return nil
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	body := map[string]any{"channel": p.Channel, "ts": p.TS, "text": p.Text}
	if err := a.slackCall(ctx, "chat.update", body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.update: %s", out.Error)
	}
	return nil
}

func (a *Activities) slackCall(ctx context.Context, method string, body any, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+a.SlackBotToken)
	resp, err := a.slackHTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack %s: http %s", method, resp.Status)
	}
	return json.Unmarshal(data, out)
}
