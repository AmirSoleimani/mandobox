package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acme/mandobox/internal/natsauth"
	"github.com/acme/mandobox/internal/reconcile"
	"github.com/acme/mandobox/internal/supervisor"
	"github.com/nats-io/nats.go"
	"go.temporal.io/sdk/activity"
)

// Activities holds the side-effecting collaborators. One instance is registered on the worker.
type Activities struct {
	Fleet   *FleetClient
	App     *GitHubApp
	NATSURL string
	// NATS decentralized-auth material (closes the unauthenticated-bus finding). When set, the worker
	// mints per-session guest creds scoped to agent.<sid>.> and connects its OWN subscriptions with the
	// broad service creds. All empty (pre-provision) → legacy unauthenticated NATS (safe in-place upgrade).
	NATSAccountSeed   string // Tier-0 account signing seed (mints per-session user creds)
	NATSAccountPubKey string // account public key (issuer-account on minted creds)
	NATSServiceCreds  string // path to the worker/bridge service .creds (agent.> pub/sub)
	GatewayURL        string // egress gateway base — the guest's LLM base_url
	BotUser           string
	BotEmail          string
	SlackBotToken     string // xoxb- token for chat.postMessage; empty → Slack posts are no-ops
	SlackChannel      string // default channel for the session thread
	// VSCodeTunnelToken is a pre-authenticated `code tunnel` token injected into guests so a human
	// attach skips the device login. Empty → operators device-login on first attach.
	VSCodeTunnelToken string
	// VSCodeTunnelHostname is the hostname that token was minted under (the CLI binds auth to the
	// hostname). The guest adopts it so the injected token validates. Normally the host's own name.
	VSCodeTunnelHostname string
	// BoxConfigPath points at the operator's mandobox.yml (defaults + guardrails). It is re-read on
	// every ResolveConfig so edits — e.g. enabling/disabling an agent in agents_allowed — take effect
	// on the next dispatch with no worker restart. Missing/invalid → built-in defaults.
	BoxConfigPath string
	// InstructionsPath points at the box-wide default agent instructions (a dashboard-managed plain
	// text file). Applied as the system-prompt addition when a repo's .mandobox.yml sets none. Empty
	// or missing → no box-wide instructions (unchanged behavior). Re-read per ResolveConfig.
	InstructionsPath string
	// Preamble override files (dashboard-managed). Read per launch and injected into MMDS; empty or
	// missing → the guest uses its built-in default preamble. Operator-only (not repo-influenced).
	PreambleAutonomousPath  string
	PreambleCollaboratePath string
	// Agent auth mode (dashboard-managed, docs/subscription-auth.md). AuthModePath holds "subscription"
	// or "api_key"; OAuthTokenPath holds the Claude subscription token (Tier-0). Read per launch; only
	// injected when the mode is "subscription". Absent/empty → the default gateway + API-key path.
	AuthModePath   string
	OAuthTokenPath string
	// ProviderConfigPath holds the dashboard-managed active-provider selection (provider.json). One
	// active provider governs the agent harness, model, and auth for every launch AND helper call.
	// Absent → the legacy AuthModePath toggle is used (safe in-place upgrade).
	ProviderConfigPath string
	// MetaDir is where LaunchVM writes a durable <session>.meta.json (how the agent ran: model,
	// provider, auth). Same dir as the nats-bridge event/log archive, so the dashboard reads all
	// three from one place. It outlives the workflow, so a closed session still shows how it ran.
	MetaDir string
	// ReconcileAuthority reports which sessions still have a Running workflow; ReconcileGrace exempts
	// just-launched VMs. Used by FindOrphanVMs (the scheduled reaper). Nil authority → reaper is a
	// no-op that errors (fail-closed), never reaps.
	ReconcileAuthority reconcile.Authority
	ReconcileGrace     time.Duration
	slackClient        *http.Client
}

// natsConnect dials the control bus, adding the worker's broad service credentials once NATS auth is
// provisioned. An unset/absent creds file → legacy unauthenticated connect, so the code is safe to
// deploy BEFORE the server cutover.
func (a *Activities) natsConnect(url string, opts ...nats.Option) (*nats.Conn, error) {
	if a.NATSServiceCreds != "" {
		if _, err := os.Stat(a.NATSServiceCreds); err == nil {
			opts = append(opts, nats.UserCredentials(a.NATSServiceCreds))
		}
	}
	return nats.Connect(url, opts...)
}

// MintCredentials issues the per-session Tier-1 tokens. The Anthropic key is never minted
// here — the guest's LLM auth token is a session handle the egress gateway exchanges for the real key
// host-side. The minted NATS creds confine the guest to its own agent.<sid>.> subtree (natsauth).
func (a *Activities) MintCredentials(ctx context.Context, in WorkflowInput) (Credentials, error) {
	// Scope the guest's GitHub token to the ONE target repo with only the permissions the agent needs
	// (push commits + open/update the PR). The guest runs untrusted repo code as root and can read the
	// token, so an installation-wide token would let one malicious repo reach every repo in the org.
	token, err := a.App.MintRepoToken(ctx, in.Repo, map[string]string{
		"contents":      "write",
		"pull_requests": "write",
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("mint github token: %w", err)
	}
	// Per-session NATS creds confine the guest to agent.<sid>.> — so it can't read, inject, or forge on
	// any other session's streams over the shared bus. Empty until the account material is provisioned
	// (legacy open bus), which keeps this a safe in-place upgrade.
	var natsCreds string
	if a.NATSAccountSeed != "" && a.NATSAccountPubKey != "" {
		natsCreds, err = natsauth.MintSessionCreds(a.NATSAccountSeed, a.NATSAccountPubKey, in.SessionID, 0)
		if err != nil {
			return Credentials{}, fmt.Errorf("mint nats creds: %w", err)
		}
	}
	return Credentials{
		GitHubToken:          token,
		LLMBaseURL:           a.GatewayURL,
		LLMAuthToken:         "sess-" + in.SessionID,
		NATSCreds:            natsCreds,
		GitHubBotUser:        a.BotUser,
		GitHubBotMail:        a.BotEmail,
		VSCodeTunnelToken:    a.VSCodeTunnelToken,
		VSCodeTunnelHostname: a.VSCodeTunnelHostname,
	}, nil
}

// ResolveConfig fetches the repo's .mandobox.yml (if any) and folds it with the box config and the
// per-task WorkflowInput into the effective, clamped configuration the workflow applies. A missing,
// unreadable, or invalid repo file degrades to defaults with a warning — never an error.
func (a *Activities) ResolveConfig(ctx context.Context, in WorkflowInput) (ResolvedConfig, error) {
	box, err := LoadBoxConfig(a.BoxConfigPath) // re-read per dispatch → config edits are instant
	var pre []string
	if err != nil {
		pre = append(pre, "box config problem ("+err.Error()+") — using built-in defaults.")
	}
	// Box-wide default instructions come from the dashboard-managed file when mandobox.yml doesn't
	// set defaults.instructions inline. Re-read per dispatch → edits are instant. Absent → no-op.
	if strings.TrimSpace(box.Defaults.Instructions) == "" {
		box.Defaults.Instructions = readTrimmedFile(a.InstructionsPath)
	}
	var repo RepoConfig
	if b, err := a.App.FetchFile(ctx, in.Repo, ".mandobox.yml", ""); err != nil {
		pre = append(pre, "couldn't read .mandobox.yml ("+err.Error()+") — using defaults.")
	} else if len(b) > 0 {
		if rc, perr := ParseRepoConfig(b); perr != nil {
			pre = append(pre, ".mandobox.yml is invalid ("+perr.Error()+") — using defaults.")
		} else {
			repo = rc
		}
	}
	res := resolveConfig(box, repo, in)
	res.Warnings = append(pre, res.Warnings...)
	return res, nil
}

// readTrimmedFile returns the trimmed contents of path, or "" if the path is empty or unreadable.
// Used for the dashboard-managed instruction/preamble override files (absent → built-in behavior).
func readTrimmedFile(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// LaunchVM builds the MMDS payload and posts a launch. ErrAtCapacity (503) is retryable.
// writeSessionMeta records how a session ran — model, provider, auth — to a durable file the
// dashboard reads (it survives the workflow closing). Best-effort: any failure is ignored, since a
// missing meta file only means the UI can't show "how it ran", never a failed launch.
func (a *Activities) writeSessionMeta(p LaunchParams, prov ResolvedProvider) {
	if a.MetaDir == "" || p.Input.SessionID == "" {
		return
	}
	b, err := json.Marshal(map[string]any{
		"session_id":   p.Input.SessionID,
		"repo":         p.Input.Repo,
		"model":        prov.Model,
		"provider":     string(prov.ID),
		"subscription": prov.Subscription,
		"harness":      prov.Harness,
		"image_sha":    p.Input.ImageSHA,
	})
	if err != nil {
		return
	}
	if err := os.MkdirAll(a.MetaDir, 0o755); err != nil {
		return
	}
	final := filepath.Join(a.MetaDir, p.Input.SessionID+".meta.json")
	tmp := final + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, final)
	}
}

func (a *Activities) LaunchVM(ctx context.Context, p LaunchParams) (LaunchResult, error) {
	if p.NATSURL == "" {
		p.NATSURL = a.NATSURL
	}
	// Operator preamble overrides are read here (activity side, not the deterministic workflow) so
	// edits take effect on the next launch. Absent/empty → the guest keeps its built-in preamble.
	p.PreambleAutonomous = readTrimmedFile(a.PreambleAutonomousPath)
	p.PreambleCollaborate = readTrimmedFile(a.PreambleCollaboratePath)
	// Resolve the box-wide active provider (dashboard-managed). It governs the agent harness, model,
	// and auth uniformly — subscription runs on the OAuth token direct to Anthropic; API-key providers
	// go through the gateway. Helper calls follow the same provider (ClassifyIntent / commitMsg), so
	// nothing diverges onto a different provider or key.
	prov := a.resolveProvider()
	p.Input.Agent = prov.Harness
	p.Input.Model = prov.Model
	p.CheapModel = prov.CheapModel
	if prov.Subscription && prov.OAuthToken != "" {
		p.Auth, p.OAuthToken = "subscription", prov.OAuthToken
	} else {
		p.Auth, p.OAuthToken = "api_key", ""
	}
	a.writeSessionMeta(p, prov) // durable record of how this session ran (best-effort)
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
// guest's pr_opened event was lost in transit.
func (a *Activities) CheckPR(ctx context.Context, p CheckPRParams) (CheckPRResult, error) {
	n, url, err := a.App.FindOpenPRByBranch(ctx, p.Repo, p.Branch)
	if err != nil {
		return CheckPRResult{}, err
	}
	return CheckPRResult{Number: n, URL: url}, nil
}

// FetchThreadParams selects the PR whose conversation to pull.
type FetchThreadParams struct {
	Repo     string `json:"repo"`
	PRNumber int    `json:"pr_number"`
}

// FetchPRThread returns the PR's full human conversation from GitHub, so the workflow can fold in
// any comment a dropped webhook never delivered. The bot's own replies are excluded.
func (a *Activities) FetchPRThread(ctx context.Context, p FetchThreadParams) ([]ThreadComment, error) {
	return a.App.FetchPRThread(ctx, p.Repo, p.PRNumber, a.BotUser)
}

// RelayParams drives RelayTunnel — where to stream the human-attach tunnel's output.
type RelayParams struct {
	SessionID string `json:"session_id"`
	Channel   string `json:"channel"`
	ThreadTS  string `json:"thread_ts"`
}

// RelayTunnel streams a human-attach tunnel to Slack: it subscribes to the guest's event stream,
// posts each tunnel line (the GitHub device-login prompt, then the vscode.dev URL) into the session
// thread, and returns the working-tree status when the guest reports it detached. Bounded by the
// activity's StartToCloseTimeout (the maximum attach length).
func (a *Activities) RelayTunnel(ctx context.Context, p RelayParams) (string, error) {
	nc, err := a.natsConnect(a.NATSURL, nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
	if err != nil {
		return "", fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	events := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe("agent."+p.SessionID+".event", events)
	if err != nil {
		return "", fmt.Errorf("subscribe events: %w", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck

	seen := map[string]bool{} // dedupe re-emitted tunnel lines so each is posted once
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case msg := <-events:
			var ev supervisor.Event
			if err := json.Unmarshal(msg.Data, &ev); err != nil {
				continue
			}
			switch ev.Type {
			case supervisor.EventTunnel:
				if ev.Info == "" || seen[ev.Info] {
					continue
				}
				seen[ev.Info] = true
				_, _ = a.PostSlack(ctx, PostSlackParams{Channel: p.Channel, ThreadTS: p.ThreadTS,
					Text: ":globe_with_meridians: " + ev.Info})
			case supervisor.EventDetached:
				return ev.Info, nil
			}
		case <-ticker.C:
			activity.RecordHeartbeat(ctx, p.SessionID)
		}
	}
}

// PostPRCommentParams posts the agent's reply back onto the PR. ReplyToID, when set, threads it
// under that inline review comment; otherwise it's a top-level PR comment.
type PostPRCommentParams struct {
	Repo      string `json:"repo"`
	PRNumber  int    `json:"pr_number"`
	Body      string `json:"body"`
	ReplyToID int64  `json:"reply_to_id,omitempty"`
}

// PostPRComment mirrors a reply into the PR — threaded under the reviewer's inline comment when
// there is one, so they see the answer right where they asked.
func (a *Activities) PostPRComment(ctx context.Context, p PostPRCommentParams) error {
	if p.ReplyToID != 0 {
		return a.App.PostReviewCommentReply(ctx, p.Repo, p.PRNumber, p.ReplyToID, p.Body)
	}
	return a.App.PostPRComment(ctx, p.Repo, p.PRNumber, p.Body)
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

// DeliverMessage publishes a command to a running guest over NATS. `claude -p` is not
// interactive, so the guest queues user_message for the next turn; abort cancels the run.
func (a *Activities) DeliverMessage(ctx context.Context, p DeliverParams) error {
	url := p.NATSURL
	if url == "" {
		url = a.NATSURL
	}
	nc, err := a.natsConnect(url, nats.Timeout(5*time.Second))
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()
	cmd := supervisor.Command{Type: p.Type, Text: p.Text, Reason: p.Reason}
	if p.Type == supervisor.CommandAttach && a.VSCodeTunnelToken != "" {
		// Deliver the shared VS Code tunnel token on-demand (over the per-session-authenticated bus,
		// to this one guest) instead of baking it into every guest's MMDS.
		cmd.Text = a.VSCodeTunnelToken
	}
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
// treat the VM as lost and return so the workflow can recover instead of hanging. The guest
// heartbeats every 30s independently of the agent (even during a long LLM call), so ~2m of
// silence reliably means it's dead — no need to wait longer, and a false positive is still
// recoverable (the workflow reconciles the PR and re-queues the feedback).
const phaseLivenessWindow = 2 * time.Minute

// RunAgentPhase subscribes to the guest's NATS event stream, heartbeats to Temporal, and
// returns the first terminal outcome (pr_opened | push_done | agent_failed | needs_input). If
// the guest stops heartbeating without a terminal event, it returns agent_failed(vm_lost).
// This keeps per-phase completions out of workflow history — nats-bridge persists the
// full log/event stream separately.
func (a *Activities) RunAgentPhase(ctx context.Context, sessionID string) (PhaseResult, error) {
	url := a.NATSURL
	nc, err := a.natsConnect(url, nats.Timeout(5*time.Second), nats.MaxReconnects(-1))
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
