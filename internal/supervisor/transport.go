package supervisor

import (
	"encoding/json"
	"fmt"

	"github.com/acme/mandobox/internal/session"
)

// Transport carries messages between the guest and the control plane. It is an interface so
// the NATS implementation can later be swapped for vsock without touching the supervisor
// (PLAN §4.4). The supervisor refuses to run if a transport can't be established: no
// transport means no observability and no abort channel (§8.1).
type Transport interface {
	Publish(subject string, data []byte) error
	Subscribe(subject string, handler func([]byte)) error
	Flush() error
	Close() error
}

// Bus wraps a Transport with the per-session subject scheme agent.<session_id>.* (§5).
// Only state transitions become events (→ Temporal signals); log lines go to storage and
// Slack directly and must not become signals (§6.3).
type Bus struct {
	t   Transport
	sid session.ID
}

// NewBus returns a Bus for a session.
func NewBus(t Transport, sid session.ID) *Bus { return &Bus{t: t, sid: sid} }

func (b *Bus) subj(leaf string) string { return b.sid.SubjectPrefix() + "." + leaf }

// Log republishes a raw agent stream-json line to agent.<sid>.log (fire-and-forget).
func (b *Bus) Log(line []byte) error { return b.t.Publish(b.subj("log"), line) }

// Heartbeat signals liveness to the control plane (§7.6/§7.7 consume this).
func (b *Bus) Heartbeat() error { return b.t.Publish(b.subj("heartbeat"), []byte("{}")) }

// Event publishes a state transition and flushes so delivery is assured before the
// supervisor proceeds (e.g. pr_opened before teardown).
func (b *Bus) Event(ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := b.t.Publish(b.subj("event"), data); err != nil {
		return err
	}
	return b.t.Flush()
}

// OnCommand subscribes to agent.<sid>.command and delivers decoded commands.
func (b *Bus) OnCommand(handler func(Command)) error {
	return b.t.Subscribe(b.subj("command"), func(data []byte) {
		var c Command
		if err := json.Unmarshal(data, &c); err == nil {
			handler(c)
		}
	})
}

// Close releases the transport.
func (b *Bus) Close() error { return b.t.Close() }

// Event types published by the guest (→ Temporal signals, §6.1).
const (
	EventPROpened    = "pr_opened"
	EventPushDone    = "push_done"
	EventAgentFailed = "agent_failed"
	EventNeedsInput  = "needs_input"
	EventSessionIdle = "session_idle" // the warm VM idled out and is powering off (§6.1 keep-alive)
	EventTunnel      = "tunnel"       // human attach: `code tunnel` output (login prompt + URL) — Info
	EventDetached    = "detached"     // human attach ended: the working-tree status — Info
)

// Event is a guest→control-plane state transition.
type Event struct {
	Type      string  `json:"type"`
	PRNumber  int     `json:"pr_number,omitempty"`
	PRURL     string  `json:"pr_url,omitempty"`
	CommitSHA string  `json:"commit_sha,omitempty"`
	Stage     string  `json:"stage,omitempty"`
	Error     string  `json:"error,omitempty"`
	Question  string  `json:"question,omitempty"`
	Reply     string  `json:"reply,omitempty"` // the agent's own words this turn, for the thread
	CostUSD   float64 `json:"cost_usd,omitempty"`
	Tokens    int     `json:"tokens,omitempty"`
	Info      string  `json:"info,omitempty"` // tunnel output (EventTunnel) or tree status (EventDetached)
}

// Command types delivered to the guest (§8.3).
const (
	CommandUserMessage = "user_message"
	CommandAbort       = "abort"
	CommandAttach      = "attach" // start `code tunnel` for a human to jump into this VM
	CommandDetach      = "detach" // stop the tunnel and report the working-tree status
)

// Command is a control-plane→guest instruction.
type Command struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Reason string `json:"reason,omitempty"`
}
