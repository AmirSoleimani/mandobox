package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Queue is the mid-task steering queue. `claude -p` is not interactive, so incoming
// user_message payloads are held and applied on the next --resume. It is
// persisted on the workspace volume, not in memory, so a VM crash does not eat an
// instruction. Each message is stored as one JSON-encoded line to survive embedded newlines.
type Queue struct {
	path string
}

// NewQueue returns a queue backed by the file at path (on the workspace volume).
func NewQueue(path string) *Queue { return &Queue{path: path} }

// Append adds a message durably.
func (q *Queue) Append(msg string) error {
	if err := os.MkdirAll(filepath.Dir(q.path), 0o700); err != nil {
		return fmt.Errorf("queue: mkdir: %w", err)
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal: %w", err)
	}
	f, err := os.OpenFile(q.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("queue: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("queue: write: %w", err)
	}
	return nil
}

// Drain returns all queued messages in order and clears the queue.
func (q *Queue) Drain() ([]string, error) {
	data, err := os.ReadFile(q.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("queue: read: %w", err)
	}
	var msgs []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var s string
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, fmt.Errorf("queue: decode: %w", err)
		}
		msgs = append(msgs, s)
	}
	if err := os.Remove(q.path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("queue: clear: %w", err)
	}
	return msgs, nil
}
