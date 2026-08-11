// Command telegram-gateway is the Telegram inbound half of the control plane. It runs on the fleet host
// and long-polls the Bot API (getUpdates — no public ingress, the Telegram analogue of slack-gateway's
// Socket Mode), translating:
//   - the /mando slash command   → start a PRWorkflow
//   - any other message in a chat → a user_message signal to that chat's running session
//
// Outbound (the messages themselves) is done by the worker's PostMessage activity via the Telegram
// Notifier. This process only translates; policy lives in the workflow. Routing is chat-scoped: a
// session's reply address is its chat, so this finds the workflow by conversation="telegram:<chat_id>".
package main

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

	"github.com/acme/mandobox/internal/control"
	"github.com/acme/mandobox/internal/session"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

const telegramAPI = "https://api.telegram.org/bot"

type gateway struct {
	token       string
	tc          client.Client
	namespace   string
	imageSHA    string
	shaFile     string
	model       string
	cheapModel  string
	baseBranch  string
	botUsername string
}

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required")
	}
	tc, err := client.Dial(client.Options{
		HostPort:  env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "fleet"),
	})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer tc.Close()

	g := &gateway{
		token: token, tc: tc,
		namespace:  env("TEMPORAL_NAMESPACE", "fleet"),
		imageSHA:   os.Getenv("IMAGE_SHA"),
		shaFile:    env("FLEET_IMAGE_SHA_FILE", "/var/lib/fleet/images/current.sha"),
		model:      env("CLAUDE_MODEL", "claude-sonnet-5"),
		cheapModel: env("CLAUDE_CHEAP_MODEL", "claude-haiku-4-5-20251001"),
		baseBranch: env("BASE_BRANCH", "main"),
	}
	if u, err := g.getMe(); err == nil {
		g.botUsername = u
		log.Printf("telegram-gateway: connected as @%s", u)
	}
	g.poll()
}

// poll long-polls getUpdates and dispatches each update. The offset (last update_id + 1) confirms
// processed updates so Telegram drops them — no local persistence needed across restarts.
func (g *gateway) poll() {
	var offset int64
	for {
		updates, err := g.getUpdates(offset, 30)
		if err != nil {
			log.Printf("getUpdates: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			g.handleUpdate(u)
		}
	}
}

func (g *gateway) handleUpdate(u update) {
	m := u.Message
	if m == nil || m.Text == "" || m.From.IsBot {
		return // ignore non-text updates and other bots (Telegram never echoes our own sends back)
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	text := strings.TrimSpace(m.Text)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if rest, ok := parseMandoCommand(text, g.botUsername); ok {
		g.reply(ctx, chatID, g.handleCommand(ctx, chatID, m.From.ID, rest))
		return
	}
	// A plain message → steer the chat's running session.
	g.routeMessage(ctx, chatID, text)
}

// handleCommand handles the body of a /mando command: an attach/detach control, or a repo + prompt to
// dispatch. Returns the text to reply with. Mirrors slack-gateway.handleSlash.
func (g *gateway) handleCommand(ctx context.Context, chatID string, fromID int64, rest string) string {
	if f := strings.Fields(rest); len(f) >= 1 && (f[0] == "attach" || f[0] == "detach") {
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		return g.handleAttachDetach(ctx, fromID, f[0], target)
	}
	repo, prompt, cheap, ok := parseDispatch(rest)
	if !ok {
		return "usage: /mando [--cheap] <owner/repo> <prompt>"
	}
	imageSHA := g.resolveImageSHA()
	if imageSHA == "" {
		return "no golden image is active — build one first"
	}
	sid, err := session.New()
	if err != nil {
		return "failed to generate a session id: " + err.Error()
	}
	model := ""
	if cheap {
		model = g.cheapModel
	}
	in := control.WorkflowInput{
		SessionID:    sid.String(),
		Repo:         repo,
		CloneURL:     "https://github.com/" + repo + ".git",
		BaseBranch:   g.baseBranch,
		Prompt:       prompt,
		ImageSHA:     imageSHA,
		Model:        model,
		Conversation: control.Conversation{Kind: "telegram", Channel: chatID},
	}
	if _, err := g.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: sid.String(), TaskQueue: control.TaskQueue,
	}, control.PRWorkflow, in); err != nil {
		return "failed to dispatch: " + err.Error()
	}
	log.Printf("dispatched %s on %s from telegram (chat %s)", sid, repo, chatID)
	return fmt.Sprintf("on it — spinning up a machine for %s 🧽", repo)
}

func (g *gateway) handleAttachDetach(ctx context.Context, fromID int64, verb, target string) string {
	if target == "" {
		return "usage: /mando " + verb + " <pr-number|session-id>"
	}
	wfID, err := g.resolveWorkflow(ctx, target)
	if err != nil {
		return "couldn't find a running session for " + target
	}
	who := strconv.FormatInt(fromID, 10)
	sig, payload := control.SignalDetach, any(control.DetachSignal{Requester: who})
	if verb == "attach" {
		sig, payload = control.SignalAttach, control.AttachSignal{Requester: who}
	}
	if err := g.tc.SignalWorkflow(ctx, wfID, "", sig, payload); err != nil {
		return verb + " failed: " + err.Error()
	}
	if verb == "attach" {
		return "attaching — watch this chat for the VS Code link."
	}
	return "detaching."
}

// routeMessage steers the chat's running session with a user_message signal. Silent when the chat has
// no active session (an ordinary chat message the bot isn't meant to act on).
func (g *gateway) routeMessage(ctx context.Context, chatID, text string) {
	wfID, err := g.findWorkflowByChat(ctx, chatID)
	if err != nil {
		return
	}
	if err := g.tc.SignalWorkflow(ctx, wfID, "", control.SignalUserMessage,
		control.UserMessageSignal{Text: text}); err != nil {
		log.Printf("signal user_message %s: %v", wfID, err)
		return
	}
	log.Printf("user_message → %s (chat %s)", wfID, chatID)
}

func (g *gateway) findWorkflowByChat(ctx context.Context, chatID string) (string, error) {
	return g.query(ctx, fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%s AND ExecutionStatus='Running'`,
		control.SAConversation, strconv.Quote("telegram:"+chatID)))
}

// resolveWorkflow turns a PR number or a session id into the running workflow's id.
func (g *gateway) resolveWorkflow(ctx context.Context, target string) (string, error) {
	if strings.HasPrefix(target, "s_") {
		return target, nil
	}
	n, err := strconv.Atoi(target)
	if err != nil {
		return "", fmt.Errorf("not a pr number or session id")
	}
	return g.query(ctx, fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%d AND ExecutionStatus='Running'`,
		control.SAPRNumber, n))
}

func (g *gateway) query(ctx context.Context, q string) (string, error) {
	resp, err := g.tc.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: g.namespace, Query: q, PageSize: 1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Executions) == 0 {
		return "", fmt.Errorf("not found")
	}
	return resp.Executions[0].Execution.WorkflowId, nil
}

// resolveImageSHA returns the golden image to launch: the pinned env value, else the active image,
// re-read each dispatch so an image rebuild is picked up without a restart.
func (g *gateway) resolveImageSHA() string {
	if g.imageSHA != "" {
		return g.imageSHA
	}
	b, err := os.ReadFile(g.shaFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---- pure parsing helpers (unit-tested) ----

// parseMandoCommand reports whether text is a /mando command (optionally /mando@botname) and returns
// the remainder after the command word.
func parseMandoCommand(text, botUsername string) (rest string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	word, tail, _ := strings.Cut(text[1:], " ")
	name, at, hasAt := strings.Cut(word, "@")
	if name != "mando" {
		return "", false
	}
	if hasAt && botUsername != "" && !strings.EqualFold(at, botUsername) {
		return "", false // addressed to a different bot in a group
	}
	return strings.TrimSpace(tail), true
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

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	From      struct {
		ID    int64 `json:"id"`
		IsBot bool  `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

func (g *gateway) getMe() (string, error) {
	var out struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	if err := g.apiGet("getMe", nil, &out); err != nil {
		return "", err
	}
	return out.Result.Username, nil
}

func (g *gateway) getUpdates(offset int64, timeout int) ([]update, error) {
	var out struct {
		OK     bool     `json:"ok"`
		Result []update `json:"result"`
	}
	q := fmt.Sprintf("?offset=%d&timeout=%d&allowed_updates=[\"message\"]", offset, timeout)
	if err := g.apiGet("getUpdates"+q, nil, &out); err != nil {
		return nil, err
	}
	return out.Result, nil
}

func (g *gateway) reply(ctx context.Context, chatID, text string) {
	if text == "" {
		return
	}
	_ = g.apiPostCtx(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text})
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (g *gateway) apiGet(methodAndQuery string, _ any, out any) error {
	// getUpdates uses a 30s long poll; give the HTTP client headroom over that.
	return g.do(context.Background(), 45*time.Second, http.MethodGet, methodAndQuery, nil, out)
}

func (g *gateway) apiPostCtx(ctx context.Context, method string, body map[string]any) error {
	var discard json.RawMessage
	return g.do(ctx, 15*time.Second, http.MethodPost, method, body, &discard)
}

func (g *gateway) do(ctx context.Context, timeout time.Duration, verb, methodAndQuery string, body map[string]any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, verb, telegramAPI+g.token+"/"+methodAndQuery, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
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
