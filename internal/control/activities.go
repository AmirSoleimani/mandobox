package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/chelodo/fleet/internal/supervisor"
	"github.com/nats-io/nats.go"
	"go.temporal.io/sdk/activity"
)

// Activities holds the side-effecting collaborators. One instance is registered on the worker.
type Activities struct {
	Fleet         *FleetClient
	App           *GitHubApp
	NATSURL       string
	GatewayURL    string // egress gateway base — the guest's LLM base_url (§9)
	BotUser       string
	BotEmail      string
	SlackBotToken string // xoxb- token for chat.postMessage; empty → Slack posts are no-ops
	SlackChannel  string // default channel for the session thread
	slackClient   *http.Client
}

// MintCredentials issues the per-session Tier-1 tokens (I1, §9). The Anthropic key is never
// minted here — the guest's LLM auth token is a session handle the egress gateway exchanges
// for the real key host-side; NATS is unauthenticated in v1 (single box).
func (a *Activities) MintCredentials(ctx context.Context, in WorkflowInput) (Credentials, error) {
	token, err := a.App.MintInstallationToken(ctx)
	if err != nil {
		return Credentials{}, fmt.Errorf("mint github token: %w", err)
	}
	return Credentials{
		GitHubToken:   token,
		LLMBaseURL:    a.GatewayURL,
		LLMAuthToken:  "sess-" + in.SessionID,
		NATSCreds:     "",
		GitHubBotUser: a.BotUser,
		GitHubBotMail: a.BotEmail,
	}, nil
}

// LaunchVM builds the MMDS payload and posts a launch. ErrAtCapacity (503) is retryable.
func (a *Activities) LaunchVM(ctx context.Context, p LaunchParams) (LaunchResult, error) {
	if p.NATSURL == "" {
		p.NATSURL = a.NATSURL
	}
	req := launchRequest{
		SessionID: p.Input.SessionID,
		ImageSHA:  p.Input.ImageSHA,
		VCPUs:     p.Input.VCPUs,
		MemMiB:    p.Input.MemMiB,
		MMDS:      buildMMDS(p),
	}
	return a.Fleet.Launch(ctx, req)
}

// CheckPRParams reconciles a branch against GitHub.
type CheckPRParams struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// CheckPRResult is the open PR found for the branch (Number 0 if none).
type CheckPRResult struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// CheckPR asks GitHub whether the branch already has an open PR — the source of truth when the
// guest's pr_opened event was lost in transit (§6).
func (a *Activities) CheckPR(ctx context.Context, p CheckPRParams) (CheckPRResult, error) {
	n, url, err := a.App.FindOpenPRByBranch(ctx, p.Repo, p.Branch)
	if err != nil {
		return CheckPRResult{}, err
	}
	return CheckPRResult{Number: n, URL: url}, nil
}

// DestroyParams selects a session and whether to also discard its workspace.
type DestroyParams struct {
	SessionID      string `json:"session_id"`
	PurgeWorkspace bool   `json:"purge_workspace"`
}

// DestroyVM stops the microVM (and optionally purges the workspace). Idempotent server-side.
func (a *Activities) DestroyVM(ctx context.Context, p DestroyParams) error {
	return a.Fleet.Destroy(ctx, p.SessionID, p.PurgeWorkspace)
}

// DeliverMessage publishes a command to a running guest over NATS (§8.3). `claude -p` is not
// interactive, so the guest queues user_message for the next turn; abort cancels the run.
func (a *Activities) DeliverMessage(ctx context.Context, p DeliverParams) error {
	url := p.NATSURL
	if url == "" {
		url = a.NATSURL
	}
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	cmd := supervisor.Command{Type: p.Type, Text: p.Text, Reason: p.Reason}
	body, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	subject := "agent." + p.SessionID + ".command"
	if err := nc.Publish(subject, body); err != nil {
		return err
	}
	return nc.Flush()
}

// phaseLivenessWindow: if no guest heartbeat arrives for this long (with no terminal event),
// treat the VM as lost. Generous enough to cover boot + clone before the first heartbeat, and a
// long quiet LLM turn. A false vm_lost is still recoverable — the workflow reconciles the PR
// against GitHub — but a wider window avoids the churn.
const phaseLivenessWindow = 5 * time.Minute

// RunAgentPhase subscribes to the guest's NATS event stream, heartbeats to Temporal, and
// returns the first terminal outcome (pr_opened | push_done | agent_failed | needs_input). If
// the guest stops heartbeating without a terminal event, it returns agent_failed(vm_lost).
// This keeps per-phase completions out of workflow history (§6.3) — nats-bridge persists the
// full log/event stream separately.
func (a *Activities) RunAgentPhase(ctx context.Context, sessionID string) (PhaseResult, error) {
	url := a.NATSURL
	nc, err := nats.Connect(url, nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
	if err != nil {
		return PhaseResult{}, fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	events := make(chan *nats.Msg, 64)
	beats := make(chan *nats.Msg, 64)
	subE, err := nc.ChanSubscribe("agent."+sessionID+".event", events)
	if err != nil {
		return PhaseResult{}, fmt.Errorf("subscribe events: %w", err)
	}
	defer subE.Unsubscribe() //nolint:errcheck
	subH, err := nc.ChanSubscribe("agent."+sessionID+".heartbeat", beats)
	if err != nil {
		return PhaseResult{}, fmt.Errorf("subscribe heartbeat: %w", err)
	}
	defer subH.Unsubscribe() //nolint:errcheck

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	lastBeat := time.Now()

	for {
		select {
		case <-ctx.Done():
			return PhaseResult{}, ctx.Err()

		case msg := <-events:
			var ev supervisor.Event
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				continue
			}
			if res, terminal := terminalFromEvent(ev); terminal {
				return res, nil
			}

		case <-beats:
			lastBeat = time.Now()

		case <-ticker.C:
			activity.RecordHeartbeat(ctx, sessionID)
			if time.Since(lastBeat) > phaseLivenessWindow {
				return PhaseResult{Outcome: supervisor.EventAgentFailed, Stage: "vm_lost",
					Error: fmt.Sprintf("no guest heartbeat for %s", time.Since(lastBeat).Round(time.Second))}, nil
			}
		}
	}
}

// terminalFromEvent maps a guest Event to a PhaseResult, reporting whether the phase is over.
func terminalFromEvent(ev supervisor.Event) (PhaseResult, bool) {
	res := PhaseResult{
		Outcome: ev.Type, PRNumber: ev.PRNumber, PRURL: ev.PRURL, CommitSHA: ev.CommitSHA,
		Stage: ev.Stage, Error: ev.Error, Question: ev.Question, Reply: ev.Reply,
		CostUSD: ev.CostUSD, Tokens: ev.Tokens,
	}
	switch ev.Type {
	case supervisor.EventPROpened, supervisor.EventPushDone, supervisor.EventAgentFailed, supervisor.EventNeedsInput:
		return res, true
	default:
		return PhaseResult{}, false
	}
}
