// Package control is the Temporal control plane: the PRWorkflow that owns a task from dispatch
// through the human review loop to workspace teardown, plus the activities that reach the
// mando-agent (mTLS) and NATS. Policy lives here and nowhere else — a core trust-boundary invariant.
package control

import "time"

// TaskQueue is where PRWorkflow and its activities are registered; the worker polls it.
const TaskQueue = "fleet-pr"

// Signal names. Guest signals are delivered by RunAgentPhase's return value, not as Temporal
// signals; the ones below arrive from outside the workflow — GitHub (webhook-rx) and Slack.
const (
	SignalReviewComment   = "review_comment"
	SignalReviewSubmitted = "review_submitted"
	SignalPRClosed        = "pr_closed"
	SignalCIStatus        = "ci_status"
	SignalUserMessage     = "user_message"
	SignalAbort           = "abort"
	SignalAttach          = "attach" // operator wants a browser VS Code into this session's VM
	SignalDetach          = "detach" // operator is done — stop the tunnel
)

// Search-attribute keys (registered on the `fleet` namespace by the temporal role).
const (
	SARepo        = "repo"
	SAPRNumber    = "pr_number"
	SAReviewRound = "review_round"
	SASlackThread = "slack_thread" // thread_ts → workflow, so slack-gateway routes replies
)

// Policy knobs — workflow input, baked nowhere else. Defaults applied in the
// workflow when zero-valued so a caller can pass an empty Policy.
type Policy struct {
	MaxReviewRounds int           `json:"max_review_rounds"`
	AutoFixCI       bool          `json:"auto_fix_ci"`
	CostCeilingUSD  float64       `json:"cost_ceiling_usd"`
	HardTTL         time.Duration `json:"hard_ttl"`
	ReviewDebounce  time.Duration `json:"review_debounce"`
	KeepAlive       time.Duration `json:"keep_alive_threshold"`
}

func (p Policy) withDefaults() Policy {
	if p.MaxReviewRounds == 0 {
		p.MaxReviewRounds = 5
	}
	if p.CostCeilingUSD == 0 {
		p.CostCeilingUSD = 15
	}
	if p.HardTTL == 0 {
		// The workflow lives as long as the PR: merge/close ends it via webhook; this is only the
		// backstop that reaps a workflow for an abandoned PR.
		p.HardTTL = 14 * 24 * time.Hour
	}
	if p.ReviewDebounce == 0 {
		// A short coalescing window, not the old 90s debounce: with a warm VM we deliver almost
		// immediately, only batching a rapid-fire burst into one turn (keep-alive).
		p.ReviewDebounce = 5 * time.Second
	}
	if p.KeepAlive == 0 {
		// Generous warm window so an active back-and-forth stays warm (the design's keep_alive_threshold).
		// A negative KeepAlive is the "never park" sentinel (keep the VM warm for the PR's life,
		// still bounded by HardTTL) — left untouched here; only the unset (0) case gets the default.
		p.KeepAlive = 15 * time.Minute
	}
	return p
}

// WorkflowInput is the dispatch payload: everything needed to launch the first VM. Credentials
// are NOT here — they are minted per-phase by the MintCredentials activity.
type WorkflowInput struct {
	SessionID  string `json:"session_id"` // also the workflow ID
	Repo       string `json:"repo"`       // owner/name
	CloneURL   string `json:"clone_url"`
	BaseBranch string `json:"base_branch"`
	Prompt     string `json:"prompt"`
	ImageSHA   string `json:"image_sha"`
	Model      string `json:"model"`
	// Agent (harness: claude | codex | …) and Instructions (per-repo system-prompt additions) come
	// from the resolved .mandobox.yml config (docs/configuration.md).
	Agent        string `json:"agent,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	VCPUs        int    `json:"vcpus"`
	MemMiB       int    `json:"mem_mib"`
	Policy       Policy `json:"policy"`
	// SlackChannel overrides the worker's default channel (set when dispatched from Slack so
	// the thread lands in the channel where /fleet was run). Empty → the default channel.
	SlackChannel string `json:"slack_channel"`
}

// State is the queryable workflow state block. Returned by the `status` query.
type State struct {
	SessionID           string   `json:"session_id"`
	Repo                string   `json:"repo"`
	BaseBranch          string   `json:"base_branch"`
	HeadBranch          string   `json:"head_branch"`
	ImageSHA            string   `json:"image_sha"`
	PRNumber            int      `json:"pr_number"`
	PRURL               string   `json:"pr_url"`
	VMState             string   `json:"vm_state"` // none|running|destroyed
	ReviewRound         int      `json:"review_round"`
	CumulativeCostUSD   float64  `json:"cumulative_cost_usd"`
	CumulativeTokens    int      `json:"cumulative_tokens"`
	PendingInstructions []string `json:"pending_instructions"`
	SlackChannel        string   `json:"slack_channel"`
	SlackThreadTS       string   `json:"slack_thread_ts"`
	Phase               string   `json:"phase"` // human-readable current step
	// CostCeilingReached latches once cumulative spend hits Policy.CostCeilingUSD: further agent turns
	// are paused (feedback still queues) so untrusted PR/Slack input can't drive unbounded LLM spend.
	CostCeilingReached bool `json:"cost_ceiling_reached,omitempty"`
}

const (
	vmNone      = "none"
	vmRunning   = "running"
	vmDestroyed = "destroyed"
)

// ---- signal payloads (from webhook-rx / Slack) ----

// ReviewCommentSignal / ReviewSubmittedSignal / PRClosedSignal / CIStatusSignal all carry a
// GitHub delivery ID; the workflow dedupes on it (GitHub redelivers).
type ReviewCommentSignal struct {
	Body       string `json:"body"`
	Author     string `json:"author"`
	Path       string `json:"path,omitempty"`       // file the inline comment is on (review comments)
	Line       int    `json:"line,omitempty"`       // line the inline comment is on
	CommentID  int64  `json:"comment_id,omitempty"` // review-comment id, so the reply threads under it
	DeliveryID string `json:"delivery_id"`
}

type ReviewSubmittedSignal struct {
	State      string `json:"state"` // e.g. "changes_requested", "approved", "commented"
	Body       string `json:"body"`
	Author     string `json:"author"`
	ReviewID   int64  `json:"review_id,omitempty"` // so the thread reconcile won't re-deliver it
	DeliveryID string `json:"delivery_id"`
}

type PRClosedSignal struct {
	Merged     bool   `json:"merged"`
	DeliveryID string `json:"delivery_id"`
}

type CIStatusSignal struct {
	Conclusion string `json:"conclusion"` // success|failure|...
	DetailsURL string `json:"details_url"`
	DeliveryID string `json:"delivery_id"`
}

type UserMessageSignal struct {
	Text string `json:"text"`
	// Attachments is the concatenated text of any files the operator dropped in the thread (a plan,
	// spec, notes). It is folded into the instruction the agent acts on, but never into the intent
	// classification — so a document that mentions "attach"/"detach" can't hijack the routing.
	Attachments string `json:"attachments,omitempty"`
}

type AbortSignal struct {
	Reason string `json:"reason"`
}

// AttachSignal / DetachSignal carry who asked, for the thread note.
type AttachSignal struct {
	Requester string `json:"requester"`
}

type DetachSignal struct {
	Requester string `json:"requester"`
}

// ---- activity I/O ----

// Credentials are the Tier-1, per-session tokens the guest holds. The Anthropic key
// is NEVER here — the egress gateway injects it host-side; the guest gets a session token.
type Credentials struct {
	GitHubToken       string `json:"github_token"`
	LLMAuthToken      string `json:"llm_auth_token"`
	LLMBaseURL        string `json:"llm_base_url"`
	NATSCreds         string `json:"nats_creds"`
	GitHubBotUser     string `json:"github_bot_user"`
	GitHubBotMail     string `json:"github_bot_email"`
	VSCodeTunnelToken string `json:"vscode_tunnel_token,omitempty"`
	// VSCodeTunnelHostname is the hostname the tunnel token was minted under. The VS Code CLI
	// binds its stored auth to the hostname, so the guest must adopt this exact name or `code
	// tunnel` treats itself as logged out and falls back to the device login.
	VSCodeTunnelHostname string `json:"vscode_tunnel_hostname,omitempty"`
}

// LaunchParams drives one LaunchVM call. Mode is "initial" or "resume"; on resume the guest
// reuses the workspace and continues the same Claude Code session.
type LaunchParams struct {
	Input        WorkflowInput
	Creds        Credentials
	Mode         string   // supervisor.ModeInitial | ModeResume
	Instructions []string // resume: the batched review feedback
	NATSURL      string
	// HeadBranch is the agent branch to push/PR — a task-derived name the workflow chose. Injected so
	// the guest uses the exact same ref the workflow reconciles by. Empty → the guest's default.
	HeadBranch string
	// Preamble overrides (operator box config, read by the LaunchVM activity from files — not by the
	// deterministic workflow). Empty → the guest uses its built-in default preamble.
	PreambleAutonomous  string
	PreambleCollaborate string
	// Agent auth (operator box config, read by the LaunchVM activity). Auth "subscription" injects the
	// OAuthToken so the guest's Claude Code runs on the operator's plan; "" / "api_key" → the gateway.
	Auth       string
	OAuthToken string
	// CheapModel is the active provider's helper model (commit messages, intent routing), passed to
	// the guest so its helper calls run on the same provider as the agent.
	CheapModel string
}

// LaunchResult is the mando-agent launch response plus the session it applies to.
type LaunchResult struct {
	Tap     string `json:"tap"`
	Chroot  string `json:"chroot"`
	GuestIP string `json:"guest_ip"`
	HostIP  string `json:"host_ip"`
}

// PhaseResult is the terminal outcome of a run, distilled from the guest's NATS event stream.
type PhaseResult struct {
	Outcome   string  `json:"outcome"` // pr_opened|push_done|agent_failed|needs_input|timeout
	PRNumber  int     `json:"pr_number"`
	PRURL     string  `json:"pr_url"`
	CommitSHA string  `json:"commit_sha"`
	Stage     string  `json:"stage"`
	Error     string  `json:"error"`
	Question  string  `json:"question"`
	Reply     string  `json:"reply"` // the agent's own words this turn, for the thread
	CostUSD   float64 `json:"cost_usd"`
	Tokens    int     `json:"tokens"`
}

// DeliverParams publishes a command to a running guest over NATS.
type DeliverParams struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // supervisor.CommandUserMessage | CommandAbort
	Text      string `json:"text"`
	Reason    string `json:"reason"`
	NATSURL   string `json:"nats_url"`
}
