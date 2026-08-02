// Package control is the Temporal control plane: the PRWorkflow that owns a task from dispatch
// through the human review loop to workspace teardown, plus the activities that reach the
// fleet-agent (mTLS) and NATS. Policy lives here and nowhere else (PLAN §6, invariant I6).
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
)

// Search-attribute keys (registered on the `fleet` namespace by the temporal role, PLAN §11).
const (
	SARepo        = "repo"
	SAPRNumber    = "pr_number"
	SAReviewRound = "review_round"
)

// Policy knobs — workflow input, baked nowhere else (PLAN §6.1). Defaults applied in the
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
		p.HardTTL = 24 * time.Hour // D2: 24h while iterating, not 14d.
	}
	if p.ReviewDebounce == 0 {
		p.ReviewDebounce = 90 * time.Second // §6.2: batch a burst of comments.
	}
	return p
}

// WorkflowInput is the dispatch payload: everything needed to launch the first VM. Credentials
// are NOT here — they are minted per-phase by the MintCredentials activity (I1, §9).
type WorkflowInput struct {
	SessionID  string `json:"session_id"` // also the workflow ID (§5)
	Repo       string `json:"repo"`       // owner/name
	CloneURL   string `json:"clone_url"`
	BaseBranch string `json:"base_branch"`
	Prompt     string `json:"prompt"`
	ImageSHA   string `json:"image_sha"`
	Model      string `json:"model"`
	VCPUs      int    `json:"vcpus"`
	MemMiB     int    `json:"mem_mib"`
	Policy     Policy `json:"policy"`
	// SlackChannel overrides the worker's default channel (set when dispatched from Slack so
	// the thread lands in the channel where /fleet was run). Empty → the default channel.
	SlackChannel string `json:"slack_channel"`
}

// State is the queryable workflow state block (PLAN §6.1). Returned by the `status` query.
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
}

const (
	vmNone      = "none"
	vmRunning   = "running"
	vmDestroyed = "destroyed"
)

// ---- signal payloads (from webhook-rx / Slack) ----

// ReviewCommentSignal / ReviewSubmittedSignal / PRClosedSignal / CIStatusSignal all carry a
// GitHub delivery ID; the workflow dedupes on it (§6.2 — GitHub redelivers).
type ReviewCommentSignal struct {
	Body       string `json:"body"`
	Author     string `json:"author"`
	DeliveryID string `json:"delivery_id"`
}

type ReviewSubmittedSignal struct {
	State      string `json:"state"` // e.g. "changes_requested", "approved", "commented"
	Body       string `json:"body"`
	Author     string `json:"author"`
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
}

type AbortSignal struct {
	Reason string `json:"reason"`
}

// ---- activity I/O ----

// Credentials are the Tier-1, per-session tokens the guest holds (I1, §9). The Anthropic key
// is NEVER here — the egress gateway injects it host-side; the guest gets a session token.
type Credentials struct {
	GitHubToken   string `json:"github_token"`
	LLMAuthToken  string `json:"llm_auth_token"`
	LLMBaseURL    string `json:"llm_base_url"`
	NATSCreds     string `json:"nats_creds"`
	GitHubBotUser string `json:"github_bot_user"`
	GitHubBotMail string `json:"github_bot_email"`
}

// LaunchParams drives one LaunchVM call. Mode is "initial" or "resume"; on resume the guest
// reuses the workspace and continues the same Claude Code session.
type LaunchParams struct {
	Input        WorkflowInput
	Creds        Credentials
	Mode         string   // supervisor.ModeInitial | ModeResume
	Instructions []string // resume: the batched review feedback
	NATSURL      string
}

// LaunchResult is the fleet-agent launch response plus the session it applies to.
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
	CostUSD   float64 `json:"cost_usd"`
	Tokens    int     `json:"tokens"`
}

// DeliverParams publishes a command to a running guest over NATS (§8.3).
type DeliverParams struct {
	SessionID string `json:"session_id"`
	Type      string `json:"type"` // supervisor.CommandUserMessage | CommandAbort
	Text      string `json:"text"`
	Reason    string `json:"reason"`
	NATSURL   string `json:"nats_url"`
}
