package connectors

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"

	"github.com/acme/mandobox/internal/control"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// slackConnector is the Slack chat connector: inbound over Socket Mode (no public ingress) and outbound
// via control's Slack Notifier. Routing is thread-scoped (conversation="slack:<thread_ts>").
type slackConnector struct {
	token     string
	appToken  string
	channel   string
	botUserID string
	api       *slack.Client
}

func newSlack() Connector {
	return &slackConnector{
		token:    os.Getenv("SLACK_BOT_TOKEN"),
		appToken: os.Getenv("SLACK_APP_TOKEN"),
		channel:  os.Getenv("SLACK_CHANNEL"),
	}
}

func (s *slackConnector) Kind() string    { return control.DefaultChatKind }
func (s *slackConnector) Configured() bool { return s.token != "" && s.appToken != "" }

func (s *slackConnector) Notifier() control.Notifier {
	if s.token == "" {
		return nil
	}
	return control.NewSlackNotifier(s.token, s.channel)
}

func (s *slackConnector) Serve(ctx context.Context, d *Dispatcher) error {
	api := slack.New(s.token, slack.OptionAppLevelToken(s.appToken))
	s.api = api
	if auth, err := api.AuthTest(); err == nil {
		s.botUserID = auth.UserID
		log.Printf("connectors/slack: connected as %s (%s)", auth.User, auth.UserID)
	}
	sm := socketmode.New(api)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-sm.Events:
				if !ok {
					return
				}
				s.handleEvent(ctx, d, sm, evt)
			}
		}
	}()
	return sm.RunContext(ctx)
}

func (s *slackConnector) handleEvent(ctx context.Context, d *Dispatcher, sm *socketmode.Client, evt socketmode.Event) {
	switch evt.Type {
	case socketmode.EventTypeSlashCommand:
		cmd, ok := evt.Data.(slack.SlashCommand)
		if !ok {
			return
		}
		ack := s.handleSlash(ctx, d, cmd)
		sm.Ack(*evt.Request, map[string]any{"response_type": "ephemeral", "text": ack})
	case socketmode.EventTypeEventsAPI:
		api, ok := evt.Data.(slackevents.EventsAPIEvent)
		if !ok {
			return
		}
		sm.Ack(*evt.Request)
		s.handleMessage(ctx, d, api)
	}
}

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

// handleSlash dispatches "/mando [--cheap] <owner/repo> <prompt>" (and attach/detach) as a new session.
func (s *slackConnector) handleSlash(ctx context.Context, d *Dispatcher, cmd slack.SlashCommand) string {
	text := strings.TrimSpace(cmd.Text)
	if f := strings.Fields(text); len(f) >= 1 && (f[0] == "attach" || f[0] == "detach") {
		target := ""
		if len(f) >= 2 {
			target = f[1]
		}
		return attachDetach(ctx, d, cmd.UserID, f[0], target)
	}
	rest := text
	cheap := false
	if r, found := strings.CutPrefix(text, "--cheap "); found {
		cheap, rest = true, strings.TrimSpace(r)
	}
	repo, prompt, split := strings.Cut(rest, " ")
	prompt = strings.TrimSpace(prompt)
	if !split || !strings.Contains(repo, "/") || prompt == "" {
		return "usage: `/mando [--cheap] <owner/repo> <prompt>`"
	}
	sid, err := d.Dispatch(ctx, control.Conversation{Kind: "slack", Channel: cmd.ChannelID}, repo, prompt, cheap)
	if err != nil {
		return "failed to dispatch: " + err.Error()
	}
	log.Printf("connectors/slack: dispatched %s on %s (%s)", sid, repo, cmd.UserID)
	return fmt.Sprintf("%s for `%s` — opening a thread here now. 🧽", giggleAck(), repo)
}

// handleMessage routes thread replies to the owning workflow as user_message signals.
func (s *slackConnector) handleMessage(ctx context.Context, d *Dispatcher, e slackevents.EventsAPIEvent) {
	if e.Type != slackevents.CallbackEvent {
		return
	}
	msg, ok := e.InnerEvent.Data.(*slackevents.MessageEvent)
	if !ok {
		return
	}
	// Ignore bot messages (including our own thread rendering) and non-thread messages.
	if msg.BotID != "" || msg.User == "" || msg.User == s.botUserID || msg.ThreadTimeStamp == "" {
		return
	}
	wfID, err := d.FindByConversation(ctx, "slack:"+msg.ThreadTimeStamp)
	if err != nil {
		return // a reply in a thread we don't own
	}
	text := strings.TrimSpace(msg.Text)
	var files []slack.File
	if msg.Message != nil {
		files = msg.Message.Files
	}
	attachments := s.readAttachedFiles(ctx, files)
	if text == "" && attachments == "" {
		return
	}
	if err := d.Signal(ctx, wfID, control.SignalUserMessage,
		control.UserMessageSignal{Text: text, Attachments: attachments}); err != nil {
		log.Printf("connectors/slack signal %s: %v", wfID, err)
		return
	}
	log.Printf("connectors/slack: user_message → %s (thread %s, %d file(s))", wfID, msg.ThreadTimeStamp, len(files))
}

const maxAttachBytes = 256 * 1024

// readAttachedFiles inlines the text files an operator dropped in a thread for the agent to read.
// Non-text/oversized files are noted and skipped; a download failure is logged and skipped, never fatal.
func (s *slackConnector) readAttachedFiles(ctx context.Context, files []slack.File) string {
	var b strings.Builder
	for _, f := range files {
		switch {
		case !isTextAttachment(f):
			fmt.Fprintf(&b, "\n\n[skipped attachment %q — not a readable text file]", f.Name)
		case f.Size > maxAttachBytes:
			fmt.Fprintf(&b, "\n\n[skipped attachment %q — too large (%d KB, cap %d KB)]", f.Name, f.Size/1024, maxAttachBytes/1024)
		default:
			var buf bytes.Buffer
			if err := s.api.GetFileContext(ctx, f.URLPrivate, &buf); err != nil {
				log.Printf("connectors/slack download attachment %s: %v", f.Name, err)
				fmt.Fprintf(&b, "\n\n[couldn't read attachment %q — is the bot's files:read scope enabled?]", f.Name)
				continue
			}
			fmt.Fprintf(&b, "\n\n--- attached file: %s ---\n%s\n--- end: %s ---", f.Name, strings.TrimRight(buf.String(), "\n"), f.Name)
		}
	}
	return strings.TrimSpace(b.String())
}

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
