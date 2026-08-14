package control_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/control"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"
)

type PRWorkflowSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
}

func TestPRWorkflow(t *testing.T) { suite.Run(t, new(PRWorkflowSuite)) }

func baseInput() control.WorkflowInput {
	return control.WorkflowInput{
		SessionID:  "s_0123456789ABCDEFGHJKMNPQRS",
		Repo:       "acme/hello-gents",
		BaseBranch: "main",
		Prompt:     "add a readme",
		ImageSHA:   "deadbeef",
	}
}

// A review comment drives exactly one resume round, and merging tears the workspace down.
func (s *PRWorkflowSuite) Test_ReviewComment_Resume_Then_Merge() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.PostMessage, mock.Anything, mock.Anything).Return(control.NotifyResult{}, nil)
	var delivered int
	env.OnActivity(a.DeliverMessage, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ control.DeliverParams) error { delivered++; return nil })
	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).
		Return(control.Credentials{GitHubToken: "t"}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).
		Return(control.LaunchResult{GuestIP: "10.0.0.2"}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "pr_opened", PRNumber: 7, PRURL: "u", CostUSD: 0.5, Tokens: 100}, nil).Once()
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "push_done", CommitSHA: "abc", CostUSD: 0.2, Tokens: 50}, nil).Once()
	env.OnActivity(a.FetchPRThread, mock.Anything, mock.Anything).Return([]control.ThreadComment{}, nil)
	var purged bool
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).
		Return(func(_ context.Context, p control.DestroyParams) error {
			if p.PurgeWorkspace {
				purged = true
			}
			return nil
		})

	// One review comment shortly after the PR opens; a duplicate delivery must be ignored.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalReviewComment,
			control.ReviewCommentSignal{Body: "please fix", Author: "amir", DeliveryID: "d1"})
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalReviewComment,
			control.ReviewCommentSignal{Body: "please fix", Author: "amir", DeliveryID: "d1"}) // dup
	}, 2*time.Millisecond)
	// Merge after the resume round has had time to run (past the 90s debounce).
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalPRClosed, control.PRClosedSignal{Merged: true, DeliveryID: "d2"})
	}, 200*time.Second)

	env.ExecuteWorkflow(control.PRWorkflow, baseInput())

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())
	var st control.State
	s.NoError(env.GetWorkflowResult(&st))
	s.Equal(7, st.PRNumber)
	s.Equal(1, st.ReviewRound, "duplicate delivery must not create a second round")
	s.Equal("merged", st.Phase)
	s.InDelta(0.7, st.CumulativeCostUSD, 0.0001)
	s.True(purged, "merging must purge the workspace")
	s.GreaterOrEqual(delivered, 1, "the review round must be delivered to the warm VM")
	env.AssertNumberOfCalls(s.T(), "LaunchVM", 1) // warm: no relaunch for the review round
	env.AssertExpectations(s.T())
}

// A Linear-conversation run advances the issue state at pr_opened (→ in_review) and at merge (→ done) via
// the Kind-gated Advance seam. A non-Linear run never calls AdvanceConversation — proven by every other
// test here staying green without mocking it (an unmocked call would fail them).
func (s *PRWorkflowSuite) Test_LinearConversation_AdvancesIssueState() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.PostMessage, mock.Anything, mock.Anything).
		Return(control.NotifyResult{Thread: "iss_1", Channel: "iss_1"}, nil)
	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).
		Return(control.Credentials{GitHubToken: "t"}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).
		Return(control.LaunchResult{GuestIP: "10.0.0.2"}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "pr_opened", PRNumber: 7, PRURL: "u", CostUSD: 0.5, Tokens: 100}, nil).Once()
	env.OnActivity(a.FetchPRThread, mock.Anything, mock.Anything).Return([]control.ThreadComment{}, nil)
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).Return(nil)

	var stages []string
	env.OnActivity(a.AdvanceConversation, mock.Anything, mock.Anything).
		Return(func(_ context.Context, p control.AdvanceParams) error {
			stages = append(stages, p.Stage)
			return nil
		})

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalPRClosed, control.PRClosedSignal{Merged: true, DeliveryID: "d1"})
	}, 200*time.Second)

	in := baseInput()
	in.Conversation = control.Conversation{Kind: "linear", Channel: "iss_1"}
	env.ExecuteWorkflow(control.PRWorkflow, in)

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())
	s.Equal([]string{"in_review", "done"}, stages, "pr_opened → in_review, merge → done")
}

// The thread reconcile folds in a human comment GitHub has but no webhook delivered (the dropped-
// delivery safety net), while a comment already delivered by webhook is not re-fed.
func (s *PRWorkflowSuite) Test_Reconcile_FoldsInMissedComment() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.PostMessage, mock.Anything, mock.Anything).Return(control.NotifyResult{}, nil)
	var texts []string
	env.OnActivity(a.DeliverMessage, mock.Anything, mock.Anything).
		Return(func(_ context.Context, p control.DeliverParams) error { texts = append(texts, p.Text); return nil })
	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).Return(control.Credentials{GitHubToken: "t"}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).Return(control.LaunchResult{GuestIP: "10.0.0.2"}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "pr_opened", PRNumber: 7, PRURL: "u", CostUSD: 0.5, Tokens: 100}, nil).Once()
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "push_done", CommitSHA: "abc", CostUSD: 0.2, Tokens: 50}, nil).Once()
	env.OnActivity(a.PostPRComment, mock.Anything, mock.Anything).Return(nil)
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).Return(nil)
	// GitHub reports two inline comments: id 10 (already delivered by webhook below) and id 55
	// (whose webhook never arrived). Only 55 should be newly folded in.
	env.OnActivity(a.FetchPRThread, mock.Anything, mock.Anything).Return([]control.ThreadComment{
		{ID: 10, Author: "amir", Body: "the delivered one", Path: "a.go", Line: 3, Kind: "review_comment", Created: "2026-01-01T00:00:00Z"},
		{ID: 55, Author: "amir", Body: "the missed one", Path: "b.go", Line: 9, Kind: "review_comment", Created: "2026-01-01T00:00:01Z"},
	}, nil)

	// One webhook-delivered inline comment (id 10) arms the round; reconcile then discovers id 55.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalReviewComment, control.ReviewCommentSignal{
			Body: "the delivered one", Author: "amir", Path: "a.go", Line: 3, CommentID: 10, DeliveryID: "d1"})
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalPRClosed, control.PRClosedSignal{Merged: true, DeliveryID: "d2"})
	}, 200*time.Second)

	env.ExecuteWorkflow(control.PRWorkflow, baseInput())

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())
	// The missed comment reached the agent; the already-delivered one was not duplicated.
	var missed, delivered10 int
	for _, t := range texts {
		if strings.Contains(t, "the missed one") {
			missed++
		}
		if strings.Contains(t, "the delivered one") {
			delivered10++
		}
	}
	s.Equal(1, missed, "the missed comment must be folded in exactly once")
	s.Equal(1, delivered10, "the already-delivered comment must not be re-fed")
}

// End-to-end through the real workflow: the text handed to PostMessage is canonical chat markup (Slack
// mrkdwn dialect). The Slack connector sends it as-is; non-Slack connectors translate it (the Telegram
// translation is covered by TestTelegramRender).
func (s *PRWorkflowSuite) Test_PostMessage_IsCanonicalChatMarkup() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	var msgs []string
	env.OnActivity(a.PostMessage, mock.Anything, mock.Anything).
		Return(func(_ context.Context, p control.PostMessageParams) (control.NotifyResult, error) {
			msgs = append(msgs, p.Text)
			return control.NotifyResult{Thread: "T1", Channel: "C1"}, nil // non-empty → replies post too
		})
	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).Return(control.Credentials{GitHubToken: "t"}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).Return(control.LaunchResult{GuestIP: "10.0.0.2"}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "pr_opened", PRNumber: 7, PRURL: "https://github.com/o/r/pull/7",
			Reply: "Fixed the **bug**.", CostUSD: 0.5, Tokens: 100}, nil).Once()
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).Return(nil)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(control.SignalPRClosed, control.PRClosedSignal{Merged: true, DeliveryID: "d"})
	}, 200*time.Second)

	env.ExecuteWorkflow(control.PRWorkflow, baseInput())
	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	joined := strings.Join(msgs, "\n---\n")
	// The workflow emits canonical chat markup (Slack mrkdwn dialect) via PostMessage; each connector
	// renders it (the Telegram translation is covered by TestTelegramRender).
	s.Contains(joined, "*PR opened*", "PR-opened announcement in the canonical dialect")
	s.Contains(joined, "<https://github.com/o/r/pull/7|#7>", "canonical <url|label> link")
}

// A run that opens no PR tears down immediately without entering the review loop.
// A first run that opens no PR no longer tears down instantly — it keeps the session so the operator
// can supply what was missing (a plan/spec). With no follow-up, the idle keep-alive ends it cleanly.
func (s *PRWorkflowSuite) Test_NoPR_WaitsThenEndsOnIdle() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.PostMessage, mock.Anything, mock.Anything).Return(control.NotifyResult{}, nil)
	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).Return(control.Credentials{}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).Return(control.LaunchResult{}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "push_done", Stage: "no_changes"}, nil).Once()
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).Return(nil)

	env.ExecuteWorkflow(control.PRWorkflow, baseInput())

	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())
	var st control.State
	s.NoError(env.GetWorkflowResult(&st))
	s.Equal(0, st.PRNumber)
	s.Equal("ended_no_input", st.Phase)
}
