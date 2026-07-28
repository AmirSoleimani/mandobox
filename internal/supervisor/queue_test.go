package supervisor

import (
	"path/filepath"
	"testing"
)

func TestQueueAppendDrain(t *testing.T) {
	q := NewQueue(filepath.Join(t.TempDir(), "sub", "queue.jsonl"))

	// Empty queue drains to nothing.
	if msgs, err := q.Drain(); err != nil || msgs != nil {
		t.Fatalf("empty drain = %v, %v", msgs, err)
	}

	for _, m := range []string{"first", "second\nwith newline", "third"} {
		if err := q.Append(m); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	msgs, err := q.Drain()
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	want := []string{"first", "second\nwith newline", "third"}
	if len(msgs) != len(want) {
		t.Fatalf("drained %d, want %d: %q", len(msgs), len(want), msgs)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg[%d] = %q, want %q", i, msgs[i], want[i])
		}
	}
	// Drain clears the queue.
	if msgs, _ := q.Drain(); msgs != nil {
		t.Fatalf("second drain = %q, want empty", msgs)
	}
}
