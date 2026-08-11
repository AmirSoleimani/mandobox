package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/control"
)

const telegramAPI = "https://api.telegram.org/bot"

// telegramConnector is the Telegram chat connector: inbound over the Bot API long-poll (getUpdates, no
// public ingress) and outbound via control's Telegram Notifier. Routing is chat-scoped
// (conversation="telegram:<chat_id>") — one active session per chat.
type telegramConnector struct {
	token       string
	defaultChat string
	client      *http.Client
	botUsername string
}

func newTelegram() Connector {
	return &telegramConnector{
		token:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		defaultChat: os.Getenv("TELEGRAM_DEFAULT_CHAT"),
		client:      &http.Client{Timeout: 45 * time.Second}, // headroom over the 30s long poll
	}
}

func (t *telegramConnector) Kind() string    { return "telegram" }
func (t *telegramConnector) Configured() bool { return t.token != "" }

func (t *telegramConnector) Notifier() control.Notifier {
	if t.token == "" {
		return nil
	}
	return control.NewTelegramNotifier(t.token, t.defaultChat)
}

func (t *telegramConnector) Serve(ctx context.Context, d *Dispatcher) error {
	if u, err := t.getMe(ctx); err == nil {
		t.botUsername = u
		log.Printf("connectors/telegram: connected as @%s", u)
	}
	var offset int64
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := t.getUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("connectors/telegram getUpdates: %v", err)
			sleepCtx(ctx, 3*time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			t.handle(ctx, d, u)
		}
	}
}

func (t *telegramConnector) handle(ctx context.Context, d *Dispatcher, u tgUpdate) {
	m := u.Message
	if m == nil || m.Text == "" || m.From.IsBot {
		return // ignore non-text updates and other bots (Telegram never echoes our own sends back)
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	text := strings.TrimSpace(m.Text)
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if cmd, rest, ok := parseSlash(text, t.botUsername); ok {
		switch strings.ToLower(cmd) {
		case "mando":
			t.reply(cctx, chatID, t.command(cctx, d, chatID, m.From.ID, rest))
			return
		case "start", "help":
			// Telegram sends /start when a user first opens the bot; without a reply it looks dead.
			t.reply(cctx, chatID, telegramWelcome)
			return
		}
		// Any other slash command falls through: it steers a running session if there is one,
		// otherwise it's ignored (or nudged, in a 1:1 chat) below.
	}
	// A plain message → steer the chat's running session.
	wfID, err := d.FindByConversation(cctx, "telegram:"+chatID)
	if err != nil {
		// No running session for this chat. In a 1:1 chat, nudge with the usage rather than
		// staying silent; in a group, stay quiet to avoid noise.
		if m.Chat.Type == "private" {
			t.reply(cctx, chatID, telegramNoSession)
		}
		return
	}
	if err := d.Signal(cctx, wfID, control.SignalUserMessage, control.UserMessageSignal{Text: text}); err != nil {
		log.Printf("connectors/telegram signal %s: %v", wfID, err)
		return
	}
	log.Printf("connectors/telegram: user_message → %s (chat %s)", wfID, chatID)
}

func (t *telegramConnector) command(ctx context.Context, d *Dispatcher, chatID string, fromID int64, rest string) string {
	if f := strings.Fields(rest); len(f) >= 1 && (f[0] == "attach" || f[0] == "detach") {
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		return attachDetach(ctx, d, strconv.FormatInt(fromID, 10), f[0], target)
	}
	repo, prompt, cheap, ok := parseDispatch(rest)
	if !ok {
		return "usage: /mando [--cheap] <owner/repo> <prompt>"
	}
	sid, err := d.Dispatch(ctx, control.Conversation{Kind: "telegram", Channel: chatID}, repo, prompt, cheap)
	if err != nil {
		return "failed to dispatch: " + err.Error()
	}
	log.Printf("connectors/telegram: dispatched %s on %s (chat %s)", sid, repo, chatID)
	return fmt.Sprintf("on it — spinning up a machine for %s 🧽", repo)
}

// ---- pure parsing helpers (unit-tested in parse_test.go) ----

// welcome / usage text. Sent as plain text (no parse_mode), so Telegram auto-links the /commands.
const telegramWelcome = "👋 I'm mandobox. Tell me a repo and what to do, and I'll spin up a machine and open a PR:\n\n" +
	"/mando <owner/repo> <what you want done>\n" +
	"e.g. /mando your-org/your-repo add a /healthz endpoint\n\n" +
	"• Put --cheap right after /mando to use a cheaper model.\n" +
	"• Once a task is running, just send messages here to steer it.\n" +
	"• /mando attach <pr-or-session> and /mando detach connect this chat to a VS Code session."

const telegramNoSession = "No task is running in this chat yet. Start one with:\n" +
	"/mando <owner/repo> <what you want done>"

// parseSlash splits a leading /command (optionally /command@botname addressed to us) from text,
// returning the command word (without the slash) and the remainder after it. ok is false when text
// isn't a slash command, or is a /command@otherbot addressed to a different bot in a group.
func parseSlash(text, botUsername string) (cmd, rest string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	word, tail, _ := strings.Cut(text[1:], " ")
	name, at, hasAt := strings.Cut(word, "@")
	if hasAt && botUsername != "" && !strings.EqualFold(at, botUsername) {
		return "", "", false // addressed to a different bot in a group
	}
	return name, strings.TrimSpace(tail), true
}

// parseDispatch splits a /mando body into repo + prompt, honouring a leading --cheap flag.
func parseDispatch(rest string) (repo, prompt string, cheap, ok bool) {
	rest = strings.TrimSpace(rest)
	if r, found := strings.CutPrefix(rest, "--cheap "); found {
		cheap, rest = true, strings.TrimSpace(r)
	}
	repo, prompt, split := strings.Cut(rest, " ")
	prompt = strings.TrimSpace(prompt)
	if !split || !strings.Contains(repo, "/") || prompt == "" {
		return "", "", false, false
	}
	return repo, prompt, cheap, true
}

// ---- Bot API ----

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type tgMessage struct {
	Text string `json:"text"`
	From struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // "private", "group", "supergroup", … — used to avoid nudging in groups
	} `json:"chat"`
}

func (t *telegramConnector) getMe(ctx context.Context) (string, error) {
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := t.do(ctx, http.MethodGet, "getMe", nil, &out); err != nil {
		return "", err
	}
	return out.Result.Username, nil
}

func (t *telegramConnector) getUpdates(ctx context.Context, offset int64, timeout int) ([]tgUpdate, error) {
	var out struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	q := fmt.Sprintf("getUpdates?offset=%d&timeout=%d&allowed_updates=[\"message\"]", offset, timeout)
	if err := t.do(ctx, http.MethodGet, q, nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

func (t *telegramConnector) reply(ctx context.Context, chatID, text string) {
	if text == "" {
		return
	}
	var discard json.RawMessage
	_ = t.do(ctx, http.MethodPost, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, &discard)
}

func (t *telegramConnector) do(ctx context.Context, verb, methodAndQuery string, body map[string]any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, verb, telegramAPI+t.token+"/"+methodAndQuery, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("telegram %s: http %s", methodAndQuery, resp.Status)
	}
	return json.Unmarshal(data, out)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
	case <-tm.C:
	}
}
