package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
)

// telegramNotifier is the Telegram chat connector's outbound half. It renders the workflow's canonical
// mrkdwn into Telegram HTML (render.go) and posts via the Bot API. Routing is chat-scoped: a session's
// Conversation.Thread is its chat id, so every message in that chat belongs to the session and the
// inbound telegram-gateway routes replies by conversation="telegram:<chat_id>". (Forum-topic threading
// for concurrent sessions in one chat is a future enhancement; today it's one active session per chat.)
type telegramNotifier struct {
	token       string
	defaultChat string
	client      *http.Client
}

// NewTelegramNotifier builds the Telegram outbound connector. token is the Bot API token; defaultChat
// is the chat id used when a Conversation carries none. Exported so the worker (a different package)
// can register it — as a plain constructor, NOT a method on Activities (an exported non-activity method
// would crash worker.RegisterActivity; see register_test.go).
func NewTelegramNotifier(token, defaultChat string) Notifier {
	return &telegramNotifier{token: token, defaultChat: defaultChat, client: &http.Client{Timeout: 15 * time.Second}}
}

func (t *telegramNotifier) Kind() string { return "telegram" }

func (t *telegramNotifier) Post(ctx context.Context, conv Conversation, text string) (NotifyResult, error) {
	if t.token == "" {
		return NotifyResult{}, nil
	}
	chat := conv.Channel
	if chat == "" {
		chat = t.defaultChat
	}
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"result"`
		Description string `json:"description"`
	}
	body := map[string]any{
		"chat_id":                  chat,
		"text":                     canonicalToTelegramHTML(text),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if err := t.call(ctx, "sendMessage", body, &out); err != nil {
		return NotifyResult{}, err
	}
	if !out.OK {
		return NotifyResult{}, fmt.Errorf("telegram sendMessage: %s", out.Description)
	}
	// Chat-scoped: the routing key is the chat id (canonical, from the API response when available), so
	// every message in this chat maps back to the session.
	channel := chat
	if out.Result.Chat.ID != 0 {
		channel = strconv.FormatInt(out.Result.Chat.ID, 10)
	}
	return NotifyResult{MessageID: strconv.FormatInt(out.Result.MessageID, 10), Thread: channel, Channel: channel}, nil
}

func (t *telegramNotifier) Update(ctx context.Context, conv Conversation, messageID, text string) error {
	if t.token == "" || messageID == "" {
		return nil
	}
	chat := conv.Channel
	if chat == "" {
		chat = t.defaultChat
	}
	id, err := strconv.Atoi(messageID)
	if err != nil {
		return nil // Telegram message ids are integers; nothing to edit otherwise
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	body := map[string]any{"chat_id": chat, "message_id": id,
		"text": canonicalToTelegramHTML(text), "parse_mode": "HTML"}
	if err := t.call(ctx, "editMessageText", body, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("telegram editMessageText: %s", out.Description)
	}
	return nil
}

// PostImage sends a PNG into the chat via the Bot API sendPhoto (multipart upload). The caption is
// plain text (no parse_mode) so it needs no escaping. Telegram routing is chat-scoped, so chat_id
// already targets the session's conversation.
func (t *telegramNotifier) PostImage(ctx context.Context, conv Conversation, caption string, png []byte, filename string) error {
	if t.token == "" || len(png) == 0 {
		return nil
	}
	chat := conv.Channel
	if chat == "" {
		chat = t.defaultChat
	}
	if filename == "" {
		filename = "preview.png"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", chat)
	if caption != "" {
		_ = mw.WriteField("caption", caption)
	}
	fw, err := mw.CreateFormFile("photo", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(png); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.token+"/sendPhoto", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := t.client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Telegram's error responses (4xx) carry the actionable detail in the JSON "description" (e.g. "Bad
	// Request: chat not found", caption too long), so parse it before the status check and surface it.
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(data, &out)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram sendPhoto: http %s: %s", resp.Status, out.Description)
	}
	if !out.OK {
		return fmt.Errorf("telegram sendPhoto: %s", out.Description)
	}
	return nil
}

func (t *telegramNotifier) call(ctx context.Context, method string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+t.token+"/"+method, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := t.client
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
		return fmt.Errorf("telegram %s: http %s", method, resp.Status)
	}
	return json.Unmarshal(data, out)
}
