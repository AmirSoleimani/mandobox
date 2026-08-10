package control_test

import (
	"context"
	"errors"
	"testing"

	"github.com/acme/mandobox/internal/control"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"
)

type ReconcileWorkflowSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
}

func TestReconcileWorkflow(t *testing.T) { suite.Run(t, new(ReconcileWorkflowSuite)) }

// Every orphan the finder returns is destroyed exactly once (workspace kept).
func (s *ReconcileWorkflowSuite) Test_ReapsEachOrphan() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.FindOrphanVMs, mock.Anything).
		Return(control.OrphanResult{Orphans: []string{"s_AAA", "s_BBB"}}, nil)
	var destroyed []control.DestroyParams
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).
		Return(func(_ context.Context, p control.DestroyParams) error {
			destroyed = append(destroyed, p)
			return nil
		})

	env.ExecuteWorkflow(control.ReconcileWorkflow)
	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())

	var res control.ReconcileResult
	s.NoError(env.GetWorkflowResult(&res))
	s.ElementsMatch([]string{"s_AAA", "s_BBB"}, res.Reaped)
	s.Len(destroyed, 2)
	for _, p := range destroyed {
		s.False(p.PurgeWorkspace, "reconcile must keep the workspace volume (I7)")
	}
}

// Fail-closed: if the finder can't establish the expected set, nothing is reaped and the run fails
// (surfaced for visibility; the next scheduled pass retries).
func (s *ReconcileWorkflowSuite) Test_FailClosed_NoReapWhenFinderErrors() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.FindOrphanVMs, mock.Anything).
		Return(control.OrphanResult{}, errors.New("temporal unreachable"))
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ control.DestroyParams) error {
			s.Fail("DestroyVM must not run when the finder fails (fail-closed)")
			return nil
		})

	env.ExecuteWorkflow(control.ReconcileWorkflow)
	s.True(env.IsWorkflowCompleted())
	s.Error(env.GetWorkflowError())
}

// No orphans → no destroys, clean completion.
func (s *ReconcileWorkflowSuite) Test_NoOrphans_NoOp() {
	env := s.NewTestWorkflowEnvironment()
	var a *control.Activities

	env.OnActivity(a.FindOrphanVMs, mock.Anything).Return(control.OrphanResult{}, nil)
	env.OnActivity(a.DestroyVM, mock.Anything, mock.Anything).
		Return(func(_ context.Context, _ control.DestroyParams) error {
			s.Fail("DestroyVM must not run when there are no orphans")
			return nil
		})

	env.ExecuteWorkflow(control.ReconcileWorkflow)
	s.True(env.IsWorkflowCompleted())
	s.NoError(env.GetWorkflowError())
	var res control.ReconcileResult
	s.NoError(env.GetWorkflowResult(&res))
	s.Empty(res.Reaped)
}
