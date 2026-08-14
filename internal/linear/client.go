// Package linear is a small client for the Linear GraphQL API + webhook verification, shared by the
// outbound notifier (internal/control) and the inbound connector (internal/connectors). It is a leaf
// package (no internal imports) so both can use it without an import cycle.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const apiURL = "https://api.linear.app/graphql"

// Client talks to the Linear GraphQL API on a personal API key. Team workflow states are cached.
type Client struct {
	apiKey string
	url    string // the GraphQL endpoint; overridable in tests
	http   *http.Client
	mu     sync.Mutex
	states map[string][]State
}

// New returns a client for the given personal API key.
func New(apiKey string) *Client {
	return &Client{apiKey: apiKey, url: apiURL, http: &http.Client{Timeout: 20 * time.Second}, states: map[string][]State{}}
}

// ---- domain types ----

// State is a Linear workflow state. Type is the stable set {triage,backlog,unstarted,started,completed,canceled}.
type State struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Label is an issue label.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Comment is an issue comment (with its author, for echo suppression).
type Comment struct {
	ID     string
	Body   string
	UserID string
}

// Issue is the slice of an issue we act on.
type Issue struct {
	ID          string
	Identifier  string
	Title       string
	Description string
	TeamID      string
	State       State
	Labels      []Label
	Comments    []Comment
}

// HasLabel reports whether the issue carries a label with this name (case-insensitive).
func (i *Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(strings.TrimSpace(l.Name), strings.TrimSpace(name)) {
			return true
		}
	}
	return false
}

// ---- transport ----

// graphql POSTs a query and decodes data into out (nil to ignore). It surfaces GraphQL errors and retries
// once on HTTP 429 honoring Retry-After.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", c.apiKey) // Linear personal keys are sent RAW, not as "Bearer"
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryAfter(resp.Header.Get("Retry-After"))):
			}
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("linear: http %s: %s", resp.Status, clip(string(raw), 200))
		}
		var env struct {
			Data   json.RawMessage   `json:"data"`
			Errors []struct{ Message string `json:"message"` } `json:"errors"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("linear: decode response: %w", err)
		}
		if len(env.Errors) > 0 {
			return fmt.Errorf("linear: %s", env.Errors[0].Message)
		}
		if out != nil {
			return json.Unmarshal(env.Data, out)
		}
		return nil
	}
}

// ---- queries / mutations ----

// Viewer returns the id of the user the API key authenticates as (used to suppress the bot's own comments).
func (c *Client) Viewer(ctx context.Context) (string, error) {
	var out struct {
		Viewer struct {
			ID string `json:"id"`
		} `json:"viewer"`
	}
	if err := c.graphql(ctx, `query { viewer { id } }`, nil, &out); err != nil {
		return "", err
	}
	return out.Viewer.ID, nil
}

// Issue fetches the canonical issue by id (labels/state/team + last 20 comments).
func (c *Client) Issue(ctx context.Context, id string) (*Issue, error) {
	const q = `query($id:String!){ issue(id:$id){ id identifier title description ` +
		`team{ id } state{ id name type } labels{ nodes{ id name } } ` +
		`comments(last:20){ nodes{ id body user{ id } } } } }`
	var out struct {
		Issue *struct {
			ID          string `json:"id"`
			Identifier  string `json:"identifier"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Team        struct {
				ID string `json:"id"`
			} `json:"team"`
			State  State `json:"state"`
			Labels struct {
				Nodes []Label `json:"nodes"`
			} `json:"labels"`
			Comments struct {
				Nodes []struct {
					ID   string `json:"id"`
					Body string `json:"body"`
					User struct {
						ID string `json:"id"`
					} `json:"user"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}
	if err := c.graphql(ctx, q, map[string]any{"id": id}, &out); err != nil {
		return nil, err
	}
	if out.Issue == nil {
		return nil, fmt.Errorf("linear: issue %q not found", id)
	}
	iss := &Issue{
		ID: out.Issue.ID, Identifier: out.Issue.Identifier, Title: out.Issue.Title,
		Description: out.Issue.Description, TeamID: out.Issue.Team.ID,
		State: out.Issue.State, Labels: out.Issue.Labels.Nodes,
	}
	for _, cm := range out.Issue.Comments.Nodes {
		iss.Comments = append(iss.Comments, Comment{ID: cm.ID, Body: cm.Body, UserID: cm.User.ID})
	}
	return iss, nil
}

// TeamStates returns a team's workflow states, cached after the first fetch.
func (c *Client) TeamStates(ctx context.Context, teamID string) ([]State, error) {
	c.mu.Lock()
	if s, ok := c.states[teamID]; ok {
		c.mu.Unlock()
		return s, nil
	}
	c.mu.Unlock()
	const q = `query($teamId:String!){ team(id:$teamId){ states{ nodes{ id name type } } } }`
	var out struct {
		Team struct {
			States struct {
				Nodes []State `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	if err := c.graphql(ctx, q, map[string]any{"teamId": teamID}, &out); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.states[teamID] = out.Team.States.Nodes
	c.mu.Unlock()
	return out.Team.States.Nodes, nil
}

// CreateComment posts a comment on an issue and returns the new comment id.
func (c *Client) CreateComment(ctx context.Context, issueID, body string) (string, error) {
	const m = `mutation($issueId:String!,$body:String!){ commentCreate(input:{issueId:$issueId,body:$body}){ success comment{ id } } }`
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	if err := c.graphql(ctx, m, map[string]any{"issueId": issueID, "body": body}, &out); err != nil {
		return "", err
	}
	if !out.CommentCreate.Success {
		return "", fmt.Errorf("linear: commentCreate returned success=false")
	}
	return out.CommentCreate.Comment.ID, nil
}

// UpdateComment edits a comment body in place; no-op on an empty id.
func (c *Client) UpdateComment(ctx context.Context, commentID, body string) error {
	if commentID == "" {
		return nil
	}
	const m = `mutation($id:String!,$body:String!){ commentUpdate(id:$id,input:{body:$body}){ success } }`
	return c.graphql(ctx, m, map[string]any{"id": commentID, "body": body}, nil)
}

// MoveState moves an issue to the workflow state for a lifecycle stage
// ("in_progress"|"in_review"|"done"|"canceled"). Best-effort: no-ops when already there or when the team
// has no matching state, and never overrides with an error that could fail the run.
func (c *Client) MoveState(ctx context.Context, issueID, stage string) error {
	iss, err := c.Issue(ctx, issueID)
	if err != nil {
		return err
	}
	states, err := c.TeamStates(ctx, iss.TeamID)
	if err != nil {
		return err
	}
	target, ok := ResolveStateID(states, stage)
	if !ok || target == iss.State.ID {
		return nil
	}
	const m = `mutation($id:String!,$stateId:String!){ issueUpdate(id:$id,input:{stateId:$stateId}){ success } }`
	return c.graphql(ctx, m, map[string]any{"id": issueID, "stateId": target}, nil)
}

// ---- state mapping ----

var stageTargets = map[string]struct {
	Type  string
	Names []string
}{
	"in_progress": {"started", []string{"in progress"}},
	"in_review":   {"started", []string{"in review", "review", "code review"}},
	"done":        {"completed", []string{"done", "merged"}},
	"canceled":    {"canceled", []string{"canceled", "cancelled", "won't do", "wont do"}},
}

// ResolveStateID picks the target state id for a lifecycle stage: a case-insensitive NAME match whose
// type also matches, else the first state of the target type, else ok=false (caller no-ops). Uses Linear's
// stable state types, so it's portable across custom per-team boards.
func ResolveStateID(states []State, stage string) (string, bool) {
	tgt, ok := stageTargets[stage]
	if !ok {
		return "", false
	}
	for _, want := range tgt.Names {
		for _, s := range states {
			if s.Type == tgt.Type && strings.EqualFold(strings.TrimSpace(s.Name), want) {
				return s.ID, true
			}
		}
	}
	for _, s := range states {
		if s.Type == tgt.Type {
			return s.ID, true
		}
	}
	return "", false
}

// ---- helpers ----

func retryAfter(h string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 && n <= 60 {
		return time.Duration(n) * time.Second
	}
	return 2 * time.Second
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
