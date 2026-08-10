package control

import "context"

// Conversation identifies the chat surface a session talks to: which connector (Kind), which channel,
// and — once the root message is posted — which thread its replies land in. This is the single seam a
// new chat connector plugs into: the inbound translator fills it in on dispatch, and the connector
// implements a Notifier for the outbound half. The workflow never names a connector; it carries a
// Conversation and calls the generic PostMessage/UpdateMessage activities.
type Conversation struct {
	Kind    string `json:"kind"`    // "slack" | "telegram" | …; "" is treated as DefaultChatKind
	Channel string `json:"channel"` // connector channel/chat id; "" → the connector's default channel
	Thread  string `json:"thread"`  // connector thread id; set by the first Post, replies target it
}

// DefaultChatKind is the connector assumed when a dispatch names none — preserving the historical
// behaviour where every session gets a thread in the default channel if that connector is configured.
const DefaultChatKind = "slack"

// resolvedKind defaults an empty Kind to DefaultChatKind.
func (c Conversation) resolvedKind() string {
	if c.Kind == "" {
		return DefaultChatKind
	}
	return c.Kind
}

// NotifyResult is what a Post returns: the posted message id (for a later edit), the thread to reply
// into (== MessageID for a root post), and the connector's canonical channel id.
type NotifyResult struct {
	MessageID string `json:"message_id"`
	Thread    string `json:"thread"`
	Channel   string `json:"channel"`
}

// Notifier is one chat connector's outbound half. Implementations are host-side (real HTTP) and are
// registered by the worker in Activities.Notifiers keyed by Kind. Adding a chat connector is: implement
// this interface, assign it into Activities.Notifiers, and write an inbound translator that starts and
// steers workflows (mirror cmd/slack-gateway). The workflow's routing and delivery never name a
// connector; the one place a connector is not yet fully abstracted is message FORMATTING (see Post).
type Notifier interface {
	// Kind is the connector this notifier handles (matches Conversation.Kind).
	Kind() string
	// Post sends text to conv. When conv.Thread is empty it starts the root message and returns the
	// new thread id; otherwise it replies within conv.Thread.
	//
	// text is the workflow's canonical chat dialect, which is currently Slack-flavoured: mrkdwn
	// (*bold*, <url|label>) plus :emoji: shortcodes. A non-Slack connector's Post is responsible for
	// translating that into its own formatting. A connector-neutral/structured message model is a
	// planned refinement; until then this is the one Slack-specific assumption a new connector inherits.
	Post(ctx context.Context, conv Conversation, text string) (NotifyResult, error)
	// Update edits a previously posted message in place (best-effort; connectors that can't may no-op).
	Update(ctx context.Context, conv Conversation, messageID, text string) error
}

// notifierFor returns the Notifier for a connector kind, or nil when none is configured (the
// PostMessage/UpdateMessage activities then no-op). Connectors are registered by the worker in the
// Activities.Notifiers map (keyed by Kind); the default Slack connector need not be — it is built
// lazily from the worker's SlackBotToken/SlackChannel so existing deploys need no extra wiring.
//
// Registration is a plain map assignment (Activities.Notifiers), NOT a method: the worker registers the
// whole Activities struct as Temporal activities via RegisterActivity, which reflects over every
// exported method and panics on one that isn't a valid activity signature — so a Register* method here
// would crash the worker at startup.
func (a *Activities) notifierFor(kind string) Notifier {
	if kind == "" {
		kind = DefaultChatKind
	}
	if n, ok := a.Notifiers[kind]; ok {
		return n
	}
	if kind == DefaultChatKind && a.SlackBotToken != "" {
		return &slackNotifier{token: a.SlackBotToken, defaultChannel: a.SlackChannel, client: a.slackHTTP()}
	}
	return nil
}

// PostMessageParams drives the generic outbound activity. The workflow calls this for every chat
// message; it routes to the Conversation's connector. An unconfigured connector is a graceful no-op.
type PostMessageParams struct {
	Conversation Conversation `json:"conversation"`
	Text         string       `json:"text"`
}

// PostMessage posts text to the session's conversation via its connector. Returns an empty result
// (no error) when the connector is unconfigured, so a task dispatched from a channel-less source
// (the dashboard/CLI) still runs.
func (a *Activities) PostMessage(ctx context.Context, p PostMessageParams) (NotifyResult, error) {
	n := a.notifierFor(p.Conversation.resolvedKind())
	if n == nil {
		return NotifyResult{}, nil
	}
	return n.Post(ctx, p.Conversation, p.Text)
}

// UpdateMessageParams edits a previously posted message.
type UpdateMessageParams struct {
	Conversation Conversation `json:"conversation"`
	MessageID    string       `json:"message_id"`
	Text         string       `json:"text"`
}

// UpdateMessage edits a message in place via the session's connector. No-op when unconfigured.
func (a *Activities) UpdateMessage(ctx context.Context, p UpdateMessageParams) error {
	n := a.notifierFor(p.Conversation.resolvedKind())
	if n == nil {
		return nil
	}
	return n.Update(ctx, p.Conversation, p.MessageID, p.Text)
}
