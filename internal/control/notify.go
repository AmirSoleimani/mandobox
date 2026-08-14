package control

import (
	"context"

	"go.temporal.io/sdk/activity"
)

// Conversation identifies the chat surface a session talks to: which connector (Kind), which channel,
// and — once the root message is posted — which thread its replies land in. This is the single seam a
// new chat connector plugs into: the inbound translator fills it in on dispatch, and the connector
// implements a Notifier for the outbound half. The workflow never names a connector; it carries a
// Conversation and calls the generic PostMessage/UpdateMessage activities.
type Conversation struct {
	Kind    string `json:"kind"`    // "slack" | "telegram" | …; "" is treated as DefaultChatKind
	Channel string `json:"channel"` // connector channel/chat id; "" → the connector's default channel
	Thread  string `json:"thread"`  // connector thread id; set by the first Post, replies target it
	// Flat marks a connector with no native threading (e.g. Telegram: every message lands in one flat
	// chat), where several sessions interleave. The workflow prefixes such a session's follow-up messages
	// with a compact session tag so they stay distinguishable. Threaded connectors (Slack) leave it false
	// — their threads already group a session's messages, and their output is byte-locked by golden tests.
	Flat bool `json:"flat,omitempty"`
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
// this interface (translating the canonical mrkdwn in Post), assign it into Activities.Notifiers, and
// write an inbound translator that starts and steers workflows (mirror cmd/slack-gateway). The workflow
// never names a connector — routing, delivery, AND message formatting are all connector-neutral.
// telegramNotifier (telegram.go) is a second, working implementation that proves the seam.
type Notifier interface {
	// Kind is the connector this notifier handles (matches Conversation.Kind).
	Kind() string
	// Post sends text to conv. When conv.Thread is empty it starts the root message and returns the
	// new thread id; otherwise it replies within conv.Thread.
	//
	// text is canonical chat markup in Slack's mrkdwn dialect (the reference dialect): *bold*, _italic_,
	// ~strike~, <url|label>, `code`, ```fences```, and :emoji: shortcodes. The Slack notifier sends it
	// as-is; every other notifier translates it — e.g. canonicalToTelegramHTML (render.go). Translators
	// are best-effort rewriters, not full parsers.
	Post(ctx context.Context, conv Conversation, text string) (NotifyResult, error)
	// Update edits a previously posted message in place (best-effort; connectors that can't may no-op).
	Update(ctx context.Context, conv Conversation, messageID, text string) error
	// PostImage uploads a PNG into conv's thread with an optional plain-text caption, returning the
	// posted message id (for reply-routing) when the connector exposes one — "" otherwise (e.g. Slack,
	// which routes replies by thread, not per-message). Best-effort: a connector that can't upload images
	// (missing scope, unsupported, empty token) returns "", nil.
	PostImage(ctx context.Context, conv Conversation, caption string, png []byte, filename string) (string, error)
	// Advance moves the conversation's external item forward for a lifecycle stage
	// ("in_review"|"done"|"canceled"). It's how an issue-tracker connector "moves the ticket along" —
	// chat connectors with no state model return nil. Best-effort: an error is swallowed by the activity.
	Advance(ctx context.Context, conv Conversation, stage string) error
}

// notifierFor returns the Notifier for a connector kind, or nil when it isn't registered — i.e. the
// connector is disabled or unconfigured (the PostMessage/UpdateMessage activities then no-op). The
// worker populates Activities.Notifiers from the enabled+configured connectors (internal/connectors) at
// startup, so enable/disable governs outbound with no lazy fallback.
//
// Registration is a plain map assignment (Activities.Notifiers), NOT a method: the worker registers the
// whole Activities struct as Temporal activities via RegisterActivity, which reflects over every
// exported method and panics on one that isn't a valid activity signature — so a Register* method here
// would crash the worker at startup.
func (a *Activities) notifierFor(kind string) Notifier {
	if kind == "" {
		kind = DefaultChatKind
	}
	return a.Notifiers[kind]
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

// PostImageParams drives the image-posting activity: a PNG (base64 over the wire) posted into the
// session's chat thread with an optional caption.
type PostImageParams struct {
	Conversation Conversation `json:"conversation"`
	Caption      string       `json:"caption"`
	PNG          []byte       `json:"png"`
	Filename     string       `json:"filename"`
}

// PostImage posts an image into the session's conversation and returns the posted message id (for
// reply-routing) when the connector exposes one, "" otherwise. It is BEST-EFFORT: a missing screenshot
// must never fail or retry the workflow, so any error (unconfigured connector, missing Slack scope,
// upload failure) is logged and swallowed — the activity always succeeds (with an empty id).
func (a *Activities) PostImage(ctx context.Context, p PostImageParams) (string, error) {
	n := a.notifierFor(p.Conversation.resolvedKind())
	if n == nil || len(p.PNG) == 0 {
		return "", nil
	}
	id, err := n.PostImage(ctx, p.Conversation, p.Caption, p.PNG, p.Filename)
	if err != nil {
		activity.GetLogger(ctx).Warn("post image failed (best-effort, ignored)", "kind", p.Conversation.resolvedKind(), "err", err)
		return "", nil
	}
	return id, nil
}

// AdvanceParams drives the lifecycle-advance activity: move the session's external item (e.g. a Linear
// issue) to the state for a lifecycle stage.
type AdvanceParams struct {
	Conversation Conversation `json:"conversation"`
	Stage        string       `json:"stage"` // "in_review" | "done" | "canceled"
}

// AdvanceConversation routes a lifecycle advance to the session's connector. BEST-EFFORT: an unconfigured
// connector, a connector with no state model (chat), or a provider error must never fail the workflow —
// the error is logged and swallowed.
func (a *Activities) AdvanceConversation(ctx context.Context, p AdvanceParams) error {
	n := a.notifierFor(p.Conversation.resolvedKind())
	if n == nil {
		return nil
	}
	if err := n.Advance(ctx, p.Conversation, p.Stage); err != nil {
		activity.GetLogger(ctx).Warn("advance conversation failed (best-effort, ignored)", "kind", p.Conversation.resolvedKind(), "stage", p.Stage, "err", err)
	}
	return nil
}
