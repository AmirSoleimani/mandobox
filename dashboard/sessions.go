package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// workflowType and statusQuery mirror the constants the worker registers (internal/control):
// PRWorkflow is the workflow function name, "status" its query handler returning State.
const (
	workflowType = "PRWorkflow"
	statusQuery  = "status"
	listPageSize = 40   // per-page fetch size when paging visibility
	maxFetch     = 1000 // safety bound on total workflows pulled across pages
	maxDisplay   = 100  // most-recent-by-activity sessions returned to the UI
	queryFanout  = 8    // concurrent "status" queries
)

// temporalStore lazily dials Temporal. The dashboard and Temporal share the box, so a dropped
// connection is recoverable on the next request rather than fatal at startup.
type temporalStore struct {
	addr, namespace string

	mu sync.Mutex
	c  client.Client
}

func newTemporalStore(addr, namespace string) *temporalStore {
	return &temporalStore{addr: addr, namespace: namespace}
}

func (s *temporalStore) conn() (client.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		return s.c, nil
	}
	c, err := client.Dial(client.Options{HostPort: s.addr, Namespace: s.namespace})
	if err != nil {
		return nil, fmt.Errorf("dial temporal %s: %w", s.addr, err)
	}
	s.c = c
	return c, nil
}

func (s *temporalStore) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c != nil {
		s.c.Close()
		s.c = nil
	}
}

// signalUserMessage is the "user_message" signal name (mirrors control.SignalUserMessage).
const signalUserMessage = "user_message"

// userMessagePayload mirrors control.UserMessageSignal — the dashboard is a separate module, so it
// re-declares the shape (json tags must match for the worker to decode it).
type userMessagePayload struct {
	Text string `json:"text"`
}

// sendUserMessage prompts a session's agent by signaling its workflow, exactly as the slack-gateway
// does. The workflow queues + coalesces the message into the next resume turn (relaunching the VM if
// it parked). Signaling a closed workflow returns an error the caller surfaces.
func (s *temporalStore) sendUserMessage(ctx context.Context, workflowID, text string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := c.SignalWorkflow(ctx, workflowID, "", signalUserMessage, userMessagePayload{Text: text}); err != nil {
		return fmt.Errorf("signal user_message: %w", err)
	}
	return nil
}

// abort signals a graceful stop (the workflow tears the VM down and ends). Mirrors control's
// SignalAbort ("abort") + AbortSignal{Reason}.
func (s *temporalStore) abort(ctx context.Context, workflowID, reason string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := c.SignalWorkflow(ctx, workflowID, "", "abort", map[string]string{"reason": reason}); err != nil {
		return fmt.Errorf("signal abort: %w", err)
	}
	return nil
}

// attach signals the workflow to bring up a browser VS Code tunnel into the session's VM. The link
// itself is surfaced by the workflow's tunnel relay (Slack thread); this only triggers it.
func (s *temporalStore) attach(ctx context.Context, workflowID string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := c.SignalWorkflow(ctx, workflowID, "", "attach", map[string]string{"requester": "dashboard"}); err != nil {
		return fmt.Errorf("signal attach: %w", err)
	}
	return nil
}

// terminate force-stops a workflow (for a wedged one that can't process a graceful abort). Unlike
// abort, this bypasses workflow code — use it only on genuinely stuck sessions.
func (s *temporalStore) terminate(ctx context.Context, workflowID, reason string) error {
	c, err := s.conn()
	if err != nil {
		return err
	}
	if err := c.TerminateWorkflow(ctx, workflowID, "", reason); err != nil {
		return fmt.Errorf("terminate: %w", err)
	}
	return nil
}

// sessions lists the most recent PRWorkflow executions (visibility) and enriches the still-live ones
// with their in-workflow State via the "status" query. Closed workflows show visibility metadata
// only — their live fields stay zero and Live=false.
func (s *temporalStore) sessions(ctx context.Context) ([]Session, error) {
	c, err := s.conn()
	if err != nil {
		return nil, err
	}

	// The box's standard visibility store can't ORDER BY and caps a single page, so we page through
	// ALL PRWorkflows (up to maxFetch) and sort client-side. Fetching a single 40-row page instead
	// dropped recently-ended sessions off the list — a workflow that started days ago but closed just
	// now sinks below the 40 most-recently-STARTED ones. maxFetch bounds an unbounded history.
	query := fmt.Sprintf("WorkflowType = '%s'", workflowType)
	var out []Session
	var token []byte
	for len(out) < maxFetch {
		resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace:     s.namespace,
			PageSize:      listPageSize,
			Query:         query,
			NextPageToken: token,
		})
		if err != nil {
			// A stale connection surfaces here; drop it so the next request redials.
			s.close()
			return nil, fmt.Errorf("list workflows: %w", err)
		}
		for _, e := range resp.GetExecutions() {
			out = append(out, visibilitySession(e))
		}
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}

	// Enrich open workflows with live State; closed ones keep visibility-only data.
	s.enrich(ctx, c, out)

	// A Running workflow whose status query didn't answer is likely wedged (a failed workflow task,
	// e.g. a nondeterminism replay error) — flag it so the UI can offer to terminate it.
	for i := range out {
		out[i].Stuck = out[i].Status == "Running" && !out[i].Live
	}

	// Order by recency of activity: Running first (they're live), then most-recently-ENDED first
	// (CloseTime for closed, StartTime for open) — so a just-closed session lands at the top instead
	// of sinking by its older start time. RFC3339 sorts lexicographically = chronologically.
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Status == "Running", out[j].Status == "Running"
		if li != lj {
			return li
		}
		return activityTime(out[i]) > activityTime(out[j])
	})

	// Cap what we return so a long history stays manageable; recency sort keeps the newest visible.
	if len(out) > maxDisplay {
		out = out[:maxDisplay]
	}
	return out, nil
}

// activityTime is the timestamp a session is ranked by: when it ended, or when it started if still open.
func activityTime(s Session) string {
	if s.CloseTime != "" {
		return s.CloseTime
	}
	return s.StartTime
}

// sessionMeta is lightweight per-session metadata from visibility (no status query).
type sessionMeta struct {
	Repo  string
	Start string // RFC3339
}

// sessionMetadata returns repo + start time per session id across all PRWorkflows (visibility-only,
// paginated). Used by the cost report to attribute archived spend to a repo/day.
func (s *temporalStore) sessionMetadata(ctx context.Context) (map[string]sessionMeta, error) {
	c, err := s.conn()
	if err != nil {
		return nil, err
	}
	out := map[string]sessionMeta{}
	query := fmt.Sprintf("WorkflowType = '%s'", workflowType)
	var token []byte
	for len(out) < maxFetch {
		resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
			Namespace: s.namespace, PageSize: listPageSize, Query: query, NextPageToken: token,
		})
		if err != nil {
			s.close()
			return nil, fmt.Errorf("list workflows: %w", err)
		}
		for _, e := range resp.GetExecutions() {
			out[e.GetExecution().GetWorkflowId()] = sessionMeta{
				Repo:  searchAttrString(e, "repo"),
				Start: tsRFC3339(e.GetStartTime().AsTime()),
			}
		}
		token = resp.GetNextPageToken()
		if len(token) == 0 {
			break
		}
	}
	return out, nil
}

// runningSessionIDs returns the set of session IDs (workflow IDs) with a Running PRWorkflow — used
// to tell live VMs apart from orphans. Visibility-only (no per-workflow queries), so it stays cheap.
func (s *temporalStore) runningSessionIDs(ctx context.Context) (map[string]bool, error) {
	c, err := s.conn()
	if err != nil {
		return nil, err
	}
	resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: s.namespace,
		PageSize:  listPageSize,
		Query:     fmt.Sprintf("WorkflowType = '%s'", workflowType),
	})
	if err != nil {
		s.close()
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	out := map[string]bool{}
	for _, e := range resp.GetExecutions() {
		if statusName(e.GetStatus()) == "Running" {
			out[e.GetExecution().GetWorkflowId()] = true
		}
	}
	return out, nil
}

func (s *temporalStore) enrich(ctx context.Context, c client.Client, sessions []Session) {
	sem := make(chan struct{}, queryFanout)
	var wg sync.WaitGroup
	for i := range sessions {
		if sessions[i].Status != "Running" {
			continue // querying closed workflows replays full history — skip for a fast, cheap list
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			st, err := queryState(qctx, c, sessions[i].WorkflowID)
			if err != nil || st == nil {
				return // leave visibility-only fields; the row still renders
			}
			mergeState(&sessions[i], st)
		}(i)
	}
	wg.Wait()
}

func queryState(ctx context.Context, c client.Client, wfID string) (*State, error) {
	val, err := c.QueryWorkflow(ctx, wfID, "", statusQuery)
	if err != nil {
		return nil, err
	}
	var st State
	if err := val.Get(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func mergeState(sess *Session, st *State) {
	sess.Live = true
	sess.Phase = st.Phase
	sess.PRNumber = st.PRNumber
	sess.PRURL = st.PRURL
	sess.VMState = st.VMState
	sess.ReviewRound = st.ReviewRound
	sess.CostUSD = st.CumulativeCostUSD
	sess.ImageSHA = st.ImageSHA
	if st.HeadBranch != "" {
		sess.Branch = st.HeadBranch
	}
	if st.Repo != "" {
		sess.Repo = st.Repo
	}
}

func visibilitySession(e *workflowpb.WorkflowExecutionInfo) Session {
	sess := Session{
		WorkflowID: e.GetExecution().GetWorkflowId(),
		RunID:      e.GetExecution().GetRunId(),
		Status:     statusName(e.GetStatus()),
		StartTime:  tsRFC3339(e.GetStartTime().AsTime()),
	}
	if e.GetCloseTime() != nil {
		sess.CloseTime = tsRFC3339(e.GetCloseTime().AsTime())
	}
	sess.Repo = searchAttrString(e, "repo")
	// The workflow upserts pr_number as a search attribute once the PR opens, so it survives the
	// session closing (the live "status" query isn't run for closed workflows). Its URL is rebuilt
	// from repo + number in the durable-enrich pass.
	sess.PRNumber = searchAttrInt(e, "pr_number")
	sess.ReviewRound = searchAttrInt(e, "review_round")
	return sess
}

// searchAttrInt pulls an Int64 search attribute. Its payload is a JSON-encoded number (e.g. "66").
func searchAttrInt(e *workflowpb.WorkflowExecutionInfo, key string) int {
	sa := e.GetSearchAttributes()
	if sa == nil {
		return 0
	}
	p, ok := sa.GetIndexedFields()[key]
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(p.GetData())))
	return n
}

// searchAttrString pulls a keyword/text search attribute as a plain string. Search-attribute
// payloads are JSON-encoded scalars, so trimming the surrounding quotes is sufficient and avoids
// wiring a full data converter for one field.
func searchAttrString(e *workflowpb.WorkflowExecutionInfo, key string) string {
	sa := e.GetSearchAttributes()
	if sa == nil {
		return ""
	}
	p, ok := sa.GetIndexedFields()[key]
	if !ok {
		return ""
	}
	raw := string(p.GetData())
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func statusName(s enumspb.WorkflowExecutionStatus) string {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return "Running"
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return "Completed"
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return "Failed"
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return "Canceled"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return "Terminated"
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return "ContinuedAsNew"
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return "TimedOut"
	default:
		return "Unknown"
	}
}

func tsRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
