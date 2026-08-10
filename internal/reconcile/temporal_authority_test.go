package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/chelodo/mandobox/internal/session"
	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
)

type fakeLister struct {
	pages []*workflowservice.ListWorkflowExecutionsResponse
	err   error
	calls int
}

func (f *fakeLister) ListWorkflow(context.Context, *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	if f.err != nil {
		return nil, f.err
	}
	p := f.pages[f.calls]
	f.calls++
	return p, nil
}

func execInfo(id string) *workflowpb.WorkflowExecutionInfo {
	return &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: id}}
}

func TestTemporalAuthorityPaginatesAndFilters(t *testing.T) {
	a, _ := session.New()
	b, _ := session.New()
	f := &fakeLister{pages: []*workflowservice.ListWorkflowExecutionsResponse{
		{Executions: []*workflowpb.WorkflowExecutionInfo{execInfo(a.String())}, NextPageToken: []byte("more")},
		{Executions: []*workflowpb.WorkflowExecutionInfo{execInfo(b.String()), execInfo("not-a-session-id")}},
	}}
	auth := NewTemporalAuthority(f, "fleet")

	got, err := auth.ExpectedSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.calls != 2 {
		t.Errorf("expected 2 pages fetched, got %d", f.calls)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 expected sessions (invalid id skipped), got %d: %v", len(got), got)
	}
	if _, ok := got[a]; !ok {
		t.Errorf("missing session %s", a)
	}
	if _, ok := got[b]; !ok {
		t.Errorf("missing session %s", b)
	}
}

func TestTemporalAuthorityFailsClosed(t *testing.T) {
	auth := NewTemporalAuthority(&fakeLister{err: errors.New("temporal down")}, "fleet")
	got, err := auth.ExpectedSessions(context.Background())
	if err == nil {
		t.Fatal("expected an error when the query fails (fail-closed), got nil")
	}
	if got != nil {
		t.Errorf("expected nil set on error, got %v", got)
	}
}
