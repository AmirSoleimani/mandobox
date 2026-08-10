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

// slackNotifier is the Slack chat connector's outbound half: one thread per session over
// chat.postMessage / chat.update. It is built from the worker's SlackBotToken (see notifierFor); when
// that is empty the connector is simply absent and posts are no-ops, so a task dispatched from the
// dashboard or CLI still runs without Slack. Inbound (the /mando command, thread replies) is handled
// by cmd/slack-gateway over Socket Mode — this half only renders the thread.
type slackNotifier struct {
	token          string
	defaultChannel string
	client         *http.Client
}

func (s *slackNotifier) Kind() string { return DefaultChatKind }

// slackHTTP is the shared client used to build the lazy Slack notifier.
func (a *Activities) slackHTTP() *http.Client {
	if a.slackClient != nil {
		return a.slackClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Post sends text to the conversation. An empty conv.Thread starts a new thread (the root message)
// and the returned Thread is that root's timestamp — the key the rest of the session replies under.
func (s *slackNotifier) Post(ctx context.Context, conv Conversation, text string) (NotifyResult, error) {
	if s.token == "" {
		return NotifyResult{}, nil
	}
	channel := conv.Channel
	if channel == "" {
		channel = s.defaultChannel
	}
	// No link previews — the thread carries a lot of URLs (PRs, tunnel links) and unfurled cards are
	// noise the operator didn't ask for.
	body := map[string]any{"channel": channel, "text": text, "unfurl_links": false, "unfurl_media": false}
	if conv.Thread != "" {
		body["thread_ts"] = conv.Thread
	}
	var out struct {
		OK      bool   `json:"ok"`
		TS      string `json:"ts"`
		Channel string `json:"channel"`
		Error   string `json:"error"`
	}
	if err := s.call(ctx, "chat.postMessage", body, &out); err != nil {
		return NotifyResult{}, err
	}
	if !out.OK {
		return NotifyResult{}, fmt.Errorf("slack chat.postMessage: %s", out.Error)
	}
	thread := conv.Thread
	if thread == "" {
		thread = out.TS // a root post: its timestamp is the thread key
	}
	channelOut := out.Channel
	if channelOut == "" {
		channelOut = channel
	}
	return NotifyResult{MessageID: out.TS, Thread: thread, Channel: channelOut}, nil
}

// Update edits a message in place (chat.update). No-op when unconfigured or messageID is empty.
func (s *slackNotifier) Update(ctx context.Context, conv Conversation, messageID, text string) error {
	if s.token == "" || messageID == "" {
		return nil
	}
	channel := conv.Channel
	if channel == "" {
		channel = s.defaultChannel
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.call(ctx, "chat.update", map[string]any{"channel": channel, "ts": messageID, "text": text}, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("slack chat.update: %s", out.Error)
	}
	return nil
}

func (s *slackNotifier) call(ctx context.Context, method string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+s.token)
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
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
