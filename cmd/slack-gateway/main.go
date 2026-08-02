// Command slack-gateway is the Slack inbound half of the control plane (PLAN §6.4). It runs on
// the fleet host and connects to Slack over Socket Mode (no public ingress), translating:
//   - the /fleet slash command  → start a PRWorkflow
//   - replies in a session thread → a user_message signal to that workflow
//
// Outbound rendering (the thread itself) is done by the worker's PostSlack activity. This
// process only translates; policy lives in the workflow.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/acme/fleet/internal/control"
	"github.com/acme/fleet/internal/session"
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
		model:      env("CLAUDE_MODEL", "claude-sonnet-5"),
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

// handleSlash dispatches "/fleet <owner/repo> <prompt>" as a new PRWorkflow.
func (g *gateway) handleSlash(cmd slack.SlashCommand) string {
	text := strings.TrimSpace(cmd.Text)
	repo, prompt, ok := strings.Cut(text, " ")
	if !ok || !strings.Contains(repo, "/") {
		return "usage: `/fleet <owner/repo> <prompt>`"
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return "usage: `/fleet <owner/repo> <prompt>` — the prompt is empty"
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
		Model:        g.model,
		VCPUs:        2,
		MemMiB:       4096,
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
	return fmt.Sprintf("Dispatched `%s` on `%s` — I'll open a thread here.", sid, repo)
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
	if text == "" {
		return
	}
	if err := g.tc.SignalWorkflow(ctx, wfID, "", control.SignalUserMessage,
		control.UserMessageSignal{Text: text}); err != nil {
		log.Printf("signal user_message %s: %v", wfID, err)
		return
	}
	log.Printf("user_message → %s (thread %s)", wfID, msg.ThreadTimeStamp)
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
