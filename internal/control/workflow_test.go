package control_test

import (
	"context"
	"testing"
	"time"

	"github.com/chelodo/fleet/internal/control"
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
		Repo:       "chelodo/hello-gents",
		BaseBranch: "main",
		Prompt:     "add a readme",
		ImageSHA:   "deadbeef",
	}
}

// A review comment drives exactly one resume round, and merging tears the workspace down.
func (s *PRWorkflowSuite) Test_ReviewComment_Resume_Then_Merge() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.MintCredentials, mock.Anything, mock.Anything).
		Return(control.Credentials{GitHubToken: "t"}, nil)
	env.OnActivity(a.LaunchVM, mock.Anything, mock.Anything).
		Return(control.LaunchResult{GuestIP: "10.0.0.2"}, nil)
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "pr_opened", PRNumber: 7, PRURL: "u", CostUSD: 0.5, Tokens: 100}, nil).Once()
	env.OnActivity(a.RunAgentPhase, mock.Anything, mock.Anything).
		Return(control.PhaseResult{Outcome: "push_done", CommitSHA: "abc", CostUSD: 0.2, Tokens: 50}, nil).Once()
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
	env.AssertExpectations(s.T())
}

// A run that opens no PR tears down immediately without entering the review loop.
func (s *PRWorkflowSuite) Test_NoPR_TearsDown() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

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
	s.Equal("no_pr", st.Phase)
}
