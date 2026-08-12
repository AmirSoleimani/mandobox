package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// NewSlackNotifier builds the Slack outbound connector from a bot token + default channel. Exported so
// the connectors registry can construct it (mirrors NewTelegramNotifier).
func NewSlackNotifier(token, defaultChannel string) Notifier {
	return &slackNotifier{token: token, defaultChannel: defaultChannel, client: &http.Client{Timeout: 15 * time.Second}}
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

// PostImage uploads a PNG into the conversation's thread via Slack's files.uploadV2 flow:
// files.getUploadURLExternal → POST the bytes to the returned URL → files.completeUploadExternal
// (which shares it into channel_id/thread_ts). Requires the bot **files:write** scope; without it
// Slack returns an error here and the caller (PostImage activity) treats it as best-effort.
func (s *slackNotifier) PostImage(ctx context.Context, conv Conversation, caption string, png []byte, filename string) error {
	if s.token == "" || len(png) == 0 {
		return nil
	}
	channel := conv.Channel
	if channel == "" {
		channel = s.defaultChannel
	}
	if filename == "" {
		filename = "preview.png"
	}

	// 1) reserve an upload URL for the bytes.
	var up struct {
		OK        bool   `json:"ok"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
		Error     string `json:"error"`
	}
	if err := s.callForm(ctx, "files.getUploadURLExternal",
		url.Values{"filename": {filename}, "length": {strconv.Itoa(len(png))}}, &up); err != nil {
		return err
	}
	if !up.OK {
		return fmt.Errorf("slack files.getUploadURLExternal: %s", up.Error)
	}

	// 2) POST the bytes to the pre-signed URL (multipart; no auth header on the upload URL itself).
	if err := s.uploadBytes(ctx, up.UploadURL, filename, png); err != nil {
		return err
	}

	// 3) finish the upload, which shares the file into the channel/thread.
	body := map[string]any{
		"files":      []map[string]string{{"id": up.FileID, "title": filename}},
		"channel_id": channel,
	}
	if conv.Thread != "" {
		body["thread_ts"] = conv.Thread
	}
	if caption != "" {
		body["initial_comment"] = caption
	}
	var done struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := s.call(ctx, "files.completeUploadExternal", body, &done); err != nil {
		return err
	}
	if !done.OK {
		return fmt.Errorf("slack files.completeUploadExternal: %s", done.Error)
	}
	return nil
}

// callForm POSTs application/x-www-form-urlencoded to a Slack Web API method (the upload handshake,
// unlike chat.postMessage, is form-encoded, not JSON).
func (s *slackNotifier) callForm(ctx context.Context, method string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://slack.com/api/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// uploadBytes POSTs raw file content to a pre-signed Slack upload URL as multipart/form-data.
func (s *slackNotifier) uploadBytes(ctx context.Context, uploadURL, filename string, content []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(content); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := s.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack upload: http %s", resp.Status)
	}
	return nil
}
