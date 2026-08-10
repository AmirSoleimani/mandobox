// Command slack-gateway is the Slack inbound half of the control plane. It runs on
// the fleet host and connects to Slack over Socket Mode (no public ingress), translating:
//   - the /mando slash command  → start a PRWorkflow
//   - replies in a session thread → a user_message signal to that workflow
//
// Outbound rendering (the thread itself) is done by the worker's PostSlack activity. This
// process only translates; policy lives in the workflow.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chelodo/mandobox/internal/control"
	"github.com/chelodo/mandobox/internal/session"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

type gateway struct {
	api        *slack.Client
	tc         client.Client
	namespace  string
	imageSHA   string // pinned via env; if empty, resolved from shaFile per dispatch
	shaFile    string // /var/lib/fleet/images/current.sha — tracks the active golden image
	model      string
	cheapModel string
	baseBranch string
	botUserID  string
}

// resolveImageSHA returns the golden image to launch: the pinned env value, else the currently
// active image, re-read each dispatch so an image rebuild is picked up without a restart.
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

func main() {
	botToken := os.Getenv("SLACK_BOT_TOKEN")
	appToken := os.Getenv("SLACK_APP_TOKEN")
	if botToken == "" || appToken == "" {
		log.Fatal("SLACK_BOT_TOKEN and SLACK_APP_TOKEN are required")
	}
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	tc, err := client.Dial(client.Options{
		HostPort:  env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "fleet"),
	})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer tc.Close()

	g := &gateway{
		api: api, tc: tc,
		namespace:  env("TEMPORAL_NAMESPACE", "fleet"),
		imageSHA:   os.Getenv("IMAGE_SHA"),
		shaFile:    env("FLEET_IMAGE_SHA_FILE", "/var/lib/fleet/images/current.sha"),
		model:      env("CLAUDE_MODEL", "claude-sonnet-5"), // real model id
		cheapModel: env("CLAUDE_CHEAP_MODEL", "claude-haiku-4-5-20251001"),
		baseBranch: env("BASE_BRANCH", "main"),
	}
	if auth, err := api.AuthTest(); err == nil {
		g.botUserID = auth.UserID
		log.Printf("slack-gateway: connected as %s (%s)", auth.User, auth.UserID)
	}

	sm := socketmode.New(api)
	go g.loop(sm)
	if err := sm.Run(); err != nil {
		log.Fatalf("socketmode: %v", err)
	}
}

func (g *gateway) loop(sm *socketmode.Client) {
	for evt := range sm.Events {
		switch evt.Type {
		case socketmode.EventTypeSlashCommand:
			cmd, ok := evt.Data.(slack.SlashCommand)
			if !ok {
				continue
			}
			ack := g.handleSlash(cmd)
			sm.Ack(*evt.Request, map[string]any{"response_type": "ephemeral", "text": ack})

		case socketmode.EventTypeEventsAPI:
			api, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			sm.Ack(*evt.Request)
			g.handleEvent(api)
		}
	}
}

// giggleAcks are the playful "on it!" openers shown the instant a task is dispatched — the same
// vibe as the dashboard's waiting fillers, so Slack feels alive before the thread even opens.
var giggleAcks = []string{
	"*giggles* spinning up a fresh machine",
	"hehehe… on it",
	"tee-hee, waking up an agent",
	"boop — booting a throwaway computer",
	"heh heh, cracking knuckles",
	"warming up the microVM",
	"*giggles* poking the brain awake",
}

func giggleAck() string { return giggleAcks[rand.IntN(len(giggleAcks))] }

// handleSlash dispatches "/mando <owner/repo> <prompt>" as a new PRWorkflow.
func (g *gateway) handleSlash(cmd slack.SlashCommand) string {
	text := strings.TrimSpace(cmd.Text)
	// Explicit fallback for the human-attach controls (you can also just say it in the thread —
	// "let me jump in" / "I'm done"). `/mando attach|detach <pr|session>`.
	if f := strings.Fields(text); len(f) >= 1 && (f[0] == "attach" || f[0] == "detach") {
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		return g.handleAttachDetach(cmd, f[0], target)
	}
	// Model & resources are left unset so the resolved config (box + repo .mandobox.yml) decides.
	// --cheap is an explicit per-task override to the cheap model class.
	model := ""
	if rest, found := strings.CutPrefix(text, "--cheap "); found {
		model, text = g.cheapModel, strings.TrimSpace(rest)
	}
	repo, prompt, ok := strings.Cut(text, " ")
	if !ok || !strings.Contains(repo, "/") {
		return "usage: `/mando [--cheap] <owner/repo> <prompt>`"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "usage: `/mando [--cheap] <owner/repo> <prompt>` — the prompt is empty"
	}
	imageSHA := g.resolveImageSHA()
	if imageSHA == "" {
		return "no golden image is active — build one first"
	}
	sid, err := session.New()
	if err != nil {
		return "failed to generate a session id: " + err.Error()
	}
	in := control.WorkflowInput{
		SessionID:    sid.String(),
		Repo:         repo,
		CloneURL:     "https://github.com/" + repo + ".git",
		BaseBranch:   g.baseBranch,
		Prompt:       prompt,
		ImageSHA:     imageSHA,
		Model:        model, // "" → config default; resources come from the resolved profile
		SlackChannel: cmd.ChannelID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = g.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: sid.String(), TaskQueue: control.TaskQueue,
	}, control.PRWorkflow, in)
	if err != nil {
		return "failed to dispatch: " + err.Error()
	}
	log.Printf("dispatched %s on %s from slack (%s)", sid, repo, cmd.UserID)
	return fmt.Sprintf("%s for `%s` — opening a thread here now. 🧽", giggleAck(), repo)
}

// handleEvent routes thread replies to the owning workflow as user_message signals.
func (g *gateway) handleEvent(e slackevents.EventsAPIEvent) {
	if e.Type != slackevents.CallbackEvent {
		return
	}
	msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Ignore bot messages (including our own thread rendering) and non-thread messages.
	if msg.BotID != "" || msg.User == "" || msg.User == g.botUserID || msg.ThreadTimeStamp == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wfID, err := g.findWorkflowByThread(ctx, msg.ThreadTimeStamp)
	if err != nil {
		return // reply in a thread we don't own — ignore
	}
	text := strings.TrimSpace(msg.Text)
	// Fold in any text files the operator dropped in the thread (a plan.md, a spec, notes) so the
	// agent reads them. They ride a separate signal field, kept out of the intent classification.
	// slack-go surfaces attachments on the normalized Message (populated even for top-level posts).
	var files []slack.File
	if msg.Message != nil {
		files = msg.Message.Files
	}
	attachments := g.readAttachedFiles(ctx, files)
	if text == "" && attachments == "" {
		return
	}
	if err := g.tc.SignalWorkflow(ctx, wfID, "", control.SignalUserMessage,
		control.UserMessageSignal{Text: text, Attachments: attachments}); err != nil {
		log.Printf("signal user_message %s: %v", wfID, err)
		return
	}
	log.Printf("user_message → %s (thread %s, %d file(s))", wfID, msg.ThreadTimeStamp, len(files))
}

// maxAttachBytes caps how much of a single attachment we fold into a signal — Temporal payloads
// should stay well under its limits, and a plan/spec/notes file is small.
const maxAttachBytes = 256 * 1024

// readAttachedFiles downloads the text attachments an operator dropped in the thread and returns
// them as labeled blocks for the agent to read. Non-text or oversized files are noted and skipped;
// a download failure is logged and skipped, never fatal. Needs the bot's `files:read` scope.
func (g *gateway) readAttachedFiles(ctx context.Context, files []slack.File) string {
	var b strings.Builder
	for _, f := range files {
		switch {
		case !isTextAttachment(f):
			fmt.Fprintf(&b, "\n\n[skipped attachment %q — not a readable text file]", f.Name)
		case f.Size > maxAttachBytes:
			fmt.Fprintf(&b, "\n\n[skipped attachment %q — too large (%d KB, cap %d KB)]", f.Name, f.Size/1024, maxAttachBytes/1024)
		default:
			var buf bytes.Buffer
			if err := g.api.GetFileContext(ctx, f.URLPrivate, &buf); err != nil {
				log.Printf("download attachment %s: %v", f.Name, err)
				fmt.Fprintf(&b, "\n\n[couldn't read attachment %q — is the bot's files:read scope enabled?]", f.Name)
				continue
			}
			fmt.Fprintf(&b, "\n\n--- attached file: %s ---\n%s\n--- end: %s ---", f.Name, strings.TrimRight(buf.String(), "\n"), f.Name)
		}
	}
	return strings.TrimSpace(b.String())
}

// isTextAttachment reports whether a Slack file is plain text we can safely inline — by mimetype or
// filename extension (plans, docs, configs, source). Images, PDFs, and binaries are excluded.
func isTextAttachment(f slack.File) bool {
	if strings.HasPrefix(f.Mimetype, "text/") {
		return true
	}
	name := strings.ToLower(f.Name)
	for _, ext := range []string{
		".md", ".markdown", ".txt", ".text", ".rst", ".json", ".yaml", ".yml", ".toml",
		".ini", ".cfg", ".conf", ".env", ".csv", ".tsv", ".log", ".xml", ".html",
		".py", ".js", ".ts", ".go", ".sh", ".bash", ".rb", ".java", ".rs", ".c", ".h", ".cpp", ".sql",
	} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// handleAttachDetach signals the owning workflow to start/stop a human-attach tunnel. The target is
// a PR number or a session id (s_…).
func (g *gateway) handleAttachDetach(cmd slack.SlashCommand, verb, target string) string {
	if target == "" {
		return "usage: `/mando " + verb + " <pr-number|session-id>`"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	wfID, err := g.resolveWorkflow(ctx, target)
	if err != nil {
		return "couldn't find a running session for `" + target + "`"
	}
	sig, payload := control.SignalDetach, any(control.DetachSignal{Requester: cmd.UserID})
	if verb == "attach" {
		sig, payload = control.SignalAttach, control.AttachSignal{Requester: cmd.UserID}
	}
	if err := g.tc.SignalWorkflow(ctx, wfID, "", sig, payload); err != nil {
		return verb + " failed: " + err.Error()
	}
	if verb == "attach" {
		return "Attaching to `" + wfID + "` — watch the thread for the VS Code link."
	}
	return "Detaching from `" + wfID + "`."
}

// resolveWorkflow turns a PR number or a session id into the running workflow's id.
func (g *gateway) resolveWorkflow(ctx context.Context, target string) (string, error) {
	if strings.HasPrefix(target, "s_") {
		return target, nil // a session id is the workflow id
	}
	n, err := strconv.Atoi(target)
	if err != nil {
		return "", fmt.Errorf("not a pr number or session id")
	}
	query := fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%d AND ExecutionStatus='Running'`,
		control.SAPRNumber, n)
	resp, err := g.tc.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: g.namespace, Query: query, PageSize: 1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Executions) == 0 {
		return "", fmt.Errorf("not found")
	}
	return resp.Executions[0].Execution.WorkflowId, nil
}

func (g *gateway) findWorkflowByThread(ctx context.Context, threadTS string) (string, error) {
	query := fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%s AND ExecutionStatus='Running'`,
		control.SASlackThread, strconv.Quote(threadTS))
	resp, err := g.tc.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: g.namespace, Query: query, PageSize: 1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Executions) == 0 {
		return "", fmt.Errorf("not found")
	}
	return resp.Executions[0].Execution.WorkflowId, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
