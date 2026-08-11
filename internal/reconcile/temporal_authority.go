package reconcile

import (
	"context"
	"fmt"

	"github.com/AmirSoleimani/mandobox/internal/session"
	"go.temporal.io/api/workflowservice/v1"
)

// workflowLister is the slice of the Temporal client the authority needs; client.Client satisfies
// it, and it lets tests inject a fake.
type workflowLister interface {
	ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error)
}

// TemporalAuthority is the reconciler authority: a VM is expected iff its session's
// PRWorkflow is still Running. It replaces the earlier FileAuthority — self-maintaining from Temporal's
// open workflows, so warm VMs a workflow is holding for keep_alive are never mistaken for orphans.
// Only VMs whose workflow has closed or vanished get reaped.
type TemporalAuthority struct {
	lister       workflowLister
	namespace    string
	workflowType string
}

// NewTemporalAuthority returns an authority backed by Temporal's Running PRWorkflows.
func NewTemporalAuthority(lister workflowLister, namespace string) *TemporalAuthority {
	return &TemporalAuthority{lister: lister, namespace: namespace, workflowType: "PRWorkflow"}
}

// ExpectedSessions returns the session IDs of all Running PRWorkflows, paging through visibility so
// none is missed (a missed session's VM would be wrongly reaped). It FAILS CLOSED: a query error is
// returned rather than a partial/empty set, so the reconciler skips the pass instead of reaping
// every VM when Temporal is unreachable.
func (a *TemporalAuthority) ExpectedSessions(ctx context.Context) (map[session.ID]struct{}, error) {
	set := map[session.ID]struct{}{}
	query := fmt.Sprintf("WorkflowType='%s' AND ExecutionStatus='Running'", a.workflowType)
	var token []byte
	for {
		resp, err := a.lister.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace:     a.namespace,
			Query:         query,
			NextPageToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list running workflows: %w", err)
		}
		for _, e := range resp.GetExecutions() {
			id, err := session.Parse(e.GetExecution().GetWorkflowId())
			if err != nil {
				continue // not a session-shaped workflow id — ignore, don't fail the whole pass
			}
			set[id] = struct{}{}
		}
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}
	return set, nil
}
