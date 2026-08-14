package linear

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// VerifySignature reports whether sig is the hex HMAC-SHA256 of body under secret, compared in constant
// time. Empty secret or signature → false (fail closed).
func VerifySignature(secret, body []byte, sig string) bool {
	if len(secret) == 0 || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// FreshTimestamp reports whether a webhook's millisecond timestamp is within skew of now — a replay
// guard per Linear's guidance. A zero timestamp (absent in the payload) passes: don't hard-fail on absence.
func FreshTimestamp(ms int64, now time.Time, skew time.Duration) bool {
	if ms == 0 {
		return true
	}
	d := now.Sub(time.UnixMilli(ms))
	if d < 0 {
		d = -d
	}
	return d <= skew
}

// Event is the outer shape of a Linear webhook delivery. Data is the entity payload, decoded on demand
// (fetch-then-decide: read only the id from Data, then fetch canonical labels/state via the API).
type Event struct {
	Action           string          `json:"action"` // create | update | remove
	Type             string          `json:"type"`   // Issue | Comment | …
	WebhookID        string          `json:"webhookId"`
	WebhookTimestamp int64           `json:"webhookTimestamp"`
	Data             json.RawMessage `json:"data"`
}

// ParseEvent decodes the outer webhook envelope.
func ParseEvent(body []byte) (Event, error) {
	var e Event
	err := json.Unmarshal(body, &e)
	return e, err
}

// IssueID pulls the issue id from an Issue event's Data.
func (e Event) IssueID() string {
	var d struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(e.Data, &d)
	return d.ID
}

// CommentInfo is the minimal comment payload we act on for steering (body is in the webhook, so no fetch).
type CommentInfo struct {
	ID      string
	Body    string
	IssueID string
	UserID  string
}

// Comment pulls the comment fields from a Comment event's Data, normalizing the id/user shapes Linear uses.
func (e Event) Comment() CommentInfo {
	var d struct {
		ID      string `json:"id"`
		Body    string `json:"body"`
		IssueID string `json:"issueId"`
		Issue   struct {
			ID string `json:"id"`
		} `json:"issue"`
		UserID string `json:"userId"`
		User   struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	_ = json.Unmarshal(e.Data, &d)
	out := CommentInfo{ID: d.ID, Body: d.Body, IssueID: d.IssueID, UserID: d.UserID}
	if out.IssueID == "" {
		out.IssueID = d.Issue.ID
	}
	if out.UserID == "" {
		out.UserID = d.User.ID
	}
	return out
}
