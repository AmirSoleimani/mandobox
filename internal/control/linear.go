package control

import (
	"context"

	"github.com/AmirSoleimani/mandobox/internal/linear"
)

// linearAPI is the slice of the Linear client the notifier uses (an interface so tests can fake it).
type linearAPI interface {
	CreateComment(ctx context.Context, issueID, body string) (string, error)
	UpdateComment(ctx context.Context, commentID, body string) error
	MoveState(ctx context.Context, issueID, stage string) error
}

// linearNotifier is the Linear connector's outbound half: it posts comments on the issue and moves the
// issue's workflow state. The Linear issue is the "thread" — conv.Channel (and conv.Thread once postRoot
// records it) are both the issue id.
type linearNotifier struct {
	client linearAPI
}

// NewLinearNotifier builds the Linear outbound notifier from a personal API key. Plain constructor (NOT a
// method on Activities) so worker.RegisterActivity doesn't mistake it for an activity — see register_test.go.
func NewLinearNotifier(apiKey string) Notifier {
	return &linearNotifier{client: linear.New(apiKey)}
}

func (l *linearNotifier) Kind() string { return "linear" }

// Post creates a comment on the issue and returns Thread=issueId so postRoot upserts the conversation
// search attribute ("linear:"+issueId) — reusing the existing reply-routing machinery.
func (l *linearNotifier) Post(ctx context.Context, conv Conversation, text string) (NotifyResult, error) {
	issueID := linearIssueID(conv)
	id, err := l.client.CreateComment(ctx, issueID, canonicalToLinearMarkdown(text))
	if err != nil {
		return NotifyResult{}, err
	}
	return NotifyResult{MessageID: id, Thread: issueID, Channel: issueID}, nil
}

// Update edits a comment in place (best-effort; no-op on empty id).
func (l *linearNotifier) Update(ctx context.Context, conv Conversation, messageID, text string) error {
	return l.client.UpdateComment(ctx, messageID, canonicalToLinearMarkdown(text))
}

// PostImage is a no-op: Linear image upload (signed-URL fileUpload flow) is a future enhancement.
func (l *linearNotifier) PostImage(context.Context, Conversation, string, []byte, string) (string, error) {
	return "", nil
}

// Advance moves the issue to the workflow state for a lifecycle stage.
func (l *linearNotifier) Advance(ctx context.Context, conv Conversation, stage string) error {
	return l.client.MoveState(ctx, linearIssueID(conv), stage)
}

func linearIssueID(conv Conversation) string {
	if conv.Channel != "" {
		return conv.Channel
	}
	return conv.Thread
}
