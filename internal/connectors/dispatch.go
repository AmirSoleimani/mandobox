package connectors

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/acme/mandobox/internal/control"
	"github.com/acme/mandobox/internal/session"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// Dispatcher is the shared Temporal-facing helper every connector's Serve loop uses to start and steer
// workflows — the generalization of what each old per-connector gateway did inline. It carries the
// dispatch defaults (image, cheap model, base branch) read from the environment.
type Dispatcher struct {
	tc         client.Client
	namespace  string
	imageSHA   string
	shaFile    string
	cheapModel string
	baseBranch string
}

func NewDispatcher(tc client.Client, namespace string) *Dispatcher {
	return &Dispatcher{
		tc: tc, namespace: namespace,
		imageSHA:   os.Getenv("IMAGE_SHA"),
		shaFile:    envOr("FLEET_IMAGE_SHA_FILE", "/var/lib/fleet/images/current.sha"),
		cheapModel: envOr("CLAUDE_CHEAP_MODEL", "claude-haiku-4-5-20251001"),
		baseBranch: envOr("BASE_BRANCH", "main"),
	}
}

// Dispatch starts a PRWorkflow for a conversation + repo + prompt. cheap selects the cheap model;
// everything else (model, resources) comes from the resolved config. Returns the new session id.
func (d *Dispatcher) Dispatch(ctx context.Context, conv control.Conversation, repo, prompt string, cheap bool) (string, error) {
	imageSHA := d.ResolveImageSHA()
	if imageSHA == "" {
		return "", fmt.Errorf("no golden image is active")
	}
	sid, err := session.New()
	if err != nil {
		return "", err
	}
	model := ""
	if cheap {
		model = d.cheapModel
	}
	in := control.WorkflowInput{
		SessionID:    sid.String(),
		Repo:         repo,
		CloneURL:     "https://github.com/" + repo + ".git",
		BaseBranch:   d.baseBranch,
		Prompt:       prompt,
		ImageSHA:     imageSHA,
		Model:        model,
		Conversation: conv,
	}
	if _, err := d.tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: sid.String(), TaskQueue: control.TaskQueue,
	}, control.PRWorkflow, in); err != nil {
		return "", err
	}
	return sid.String(), nil
}

func (d *Dispatcher) Signal(ctx context.Context, wfID, name string, payload any) error {
	return d.tc.SignalWorkflow(ctx, wfID, "", name, payload)
}

// FindByConversation returns the running workflow whose conversation search attribute equals key
// ("<kind>:<thread>", e.g. "slack:1786…" or "telegram:12345").
func (d *Dispatcher) FindByConversation(ctx context.Context, key string) (string, error) {
	return d.query(ctx, fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%s AND ExecutionStatus='Running'`,
		control.SAConversation, strconv.Quote(key)))
}

// FindByTarget resolves a PR number or a session id (s_…) to a running workflow id.
func (d *Dispatcher) FindByTarget(ctx context.Context, target string) (string, error) {
	if strings.HasPrefix(target, "s_") {
		return target, nil
	}
	n, err := strconv.Atoi(target)
	if err != nil {
		return "", fmt.Errorf("not a pr number or session id")
	}
	return d.query(ctx, fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s=%d AND ExecutionStatus='Running'`,
		control.SAPRNumber, n))
}

func (d *Dispatcher) query(ctx context.Context, q string) (string, error) {
	resp, err := d.tc.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: d.namespace, Query: q, PageSize: 1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Executions) == 0 {
		return "", fmt.Errorf("not found")
	}
	return resp.Executions[0].Execution.WorkflowId, nil
}

// ResolveImageSHA returns the golden image to launch: the pinned env value, else the active image,
// re-read each dispatch so an image rebuild is picked up without a restart.
func (d *Dispatcher) ResolveImageSHA() string {
	if d.imageSHA != "" {
		return d.imageSHA
	}
	b, err := os.ReadFile(d.shaFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// attachDetach signals the owning workflow to start/stop a human-attach tunnel. Shared by connectors;
// target is a PR number or a session id. Returns the text to reply with.
func attachDetach(ctx context.Context, d *Dispatcher, requester, verb, target string) string {
	if target == "" {
		return "usage: /mando " + verb + " <pr-number|session-id>"
	}
	wfID, err := d.FindByTarget(ctx, target)
	if err != nil {
		return "couldn't find a running session for " + target
	}
	sig, payload := control.SignalDetach, any(control.DetachSignal{Requester: requester})
	if verb == "attach" {
		sig, payload = control.SignalAttach, control.AttachSignal{Requester: requester}
	}
	if err := d.Signal(ctx, wfID, sig, payload); err != nil {
		return verb + " failed: " + err.Error()
	}
	if verb == "attach" {
		return "attaching — watch for the VS Code link."
	}
	return "detaching."
}
