package main

// state.go mirrors the fields of control.State that the dashboard displays. The dashboard is a
// separate Go module (its own go.mod) and deliberately does NOT import internal/control — it decodes
// the workflow's "status" query result into this local struct. Keep the json tags in sync with
// internal/control/types.go State; adding a field there is backward-compatible (unknown fields are
// ignored on decode), so this only needs updating when the dashboard wants to show something new.
type State struct {
	SessionID         string  `json:"session_id"`
	Repo              string  `json:"repo"`
	BaseBranch        string  `json:"base_branch"`
	HeadBranch        string  `json:"head_branch"`
	ImageSHA          string  `json:"image_sha"`
	PRNumber          int     `json:"pr_number"`
	PRURL             string  `json:"pr_url"`
	VMState           string  `json:"vm_state"` // none|running|destroyed
	ReviewRound       int     `json:"review_round"`
	CumulativeCostUSD float64 `json:"cumulative_cost_usd"`
	CumulativeTokens  int     `json:"cumulative_tokens"`
	SlackChannel      string  `json:"slack_channel"`
	SlackThreadTS     string  `json:"slack_thread_ts"`
	Phase             string  `json:"phase"`
}

// Session is the flattened view the API returns for one workflow: the visibility metadata (always
// available) merged with the live State from the "status" query (best-effort — nil for closed
// workflows whose history has aged out).
type Session struct {
	WorkflowID  string  `json:"workflow_id"`
	RunID       string  `json:"run_id"`
	Status      string  `json:"status"` // Running|Completed|Failed|Terminated|Canceled|TimedOut|ContinuedAsNew
	StartTime   string  `json:"start_time"`
	CloseTime   string  `json:"close_time,omitempty"`
	Repo        string  `json:"repo"`
	Phase       string  `json:"phase,omitempty"`
	Branch      string  `json:"branch,omitempty"`
	PRNumber    int     `json:"pr_number,omitempty"`
	PRURL       string  `json:"pr_url,omitempty"`
	VMState     string  `json:"vm_state,omitempty"`
	ReviewRound int     `json:"review_round,omitempty"`
	CostUSD     float64 `json:"cost_usd,omitempty"`
	ImageSHA    string  `json:"image_sha,omitempty"`
	Live        bool    `json:"live"`  // true if the status query answered (State fields are populated)
	Stuck       bool    `json:"stuck"` // Running but its status query failed — likely a wedged workflow task
	// How the agent ran, from the durable per-session meta the worker writes at launch (meta.go).
	// Survives the workflow closing, so a finished session still shows what it used.
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"` // claude_api | claude_subscription | codex
	Subscription bool   `json:"subscription,omitempty"`
}
