package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveStateID(t *testing.T) {
	states := []State{
		{ID: "s-backlog", Name: "Backlog", Type: "backlog"},
		{ID: "s-todo", Name: "Todo", Type: "unstarted"},
		{ID: "s-prog", Name: "In Progress", Type: "started"},
		{ID: "s-review", Name: "In Review", Type: "started"},
		{ID: "s-done", Name: "Done", Type: "completed"},
		{ID: "s-cancel", Name: "Canceled", Type: "canceled"},
	}
	cases := []struct {
		stage, want string
		ok          bool
	}{
		{"in_progress", "s-prog", true},   // name match
		{"in_review", "s-review", true},   // name match, distinct started sub-state
		{"done", "s-done", true},          // name match
		{"canceled", "s-cancel", true},    // name match
		{"bogus", "", false},              // unknown stage
	}
	for _, c := range cases {
		got, ok := ResolveStateID(states, c.stage)
		if ok != c.ok || got != c.want {
			t.Errorf("ResolveStateID(%q) = (%q,%v), want (%q,%v)", c.stage, got, ok, c.want, c.ok)
		}
	}

	// Type fallback: a board with only a generic "started" state (no "In Review") → in_review falls back
	// to that started state rather than failing.
	minimal := []State{{ID: "x", Name: "Doing", Type: "started"}, {ID: "y", Name: "Shipped", Type: "completed"}}
	if got, ok := ResolveStateID(minimal, "in_review"); !ok || got != "x" {
		t.Errorf("in_review type-fallback = (%q,%v), want (x,true)", got, ok)
	}
	// No state of the target type → no-op (ok=false, caller skips the move).
	noCancel := []State{{ID: "a", Name: "Todo", Type: "unstarted"}}
	if _, ok := ResolveStateID(noCancel, "canceled"); ok {
		t.Error("canceled with no canceled-type state should be ok=false")
	}
}

func TestVerifySignature(t *testing.T) {
	secret := []byte("sh-secret")
	body := []byte(`{"action":"update","type":"Issue"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))

	if !VerifySignature(secret, body, good) {
		t.Error("valid signature rejected")
	}
	if VerifySignature(secret, body, good+"00") {
		t.Error("tampered signature accepted")
	}
	if VerifySignature(secret, append(body, '!'), good) {
		t.Error("tampered body accepted")
	}
	if VerifySignature(nil, body, good) || VerifySignature(secret, body, "") {
		t.Error("empty secret/signature must fail closed")
	}
}

func TestFreshTimestamp(t *testing.T) {
	now := time.Now()
	if !FreshTimestamp(now.UnixMilli(), now, time.Minute) {
		t.Error("fresh timestamp rejected")
	}
	if FreshTimestamp(now.Add(-5*time.Minute).UnixMilli(), now, time.Minute) {
		t.Error("stale timestamp accepted")
	}
	if !FreshTimestamp(0, now, time.Minute) {
		t.Error("absent (0) timestamp should pass")
	}
}

func TestGraphQLErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
	}))
	defer srv.Close()
	c := New("k")
	c.url = srv.URL
	if _, err := c.Viewer(context.Background()); err == nil || err.Error() != "linear: Authentication required" {
		t.Fatalf("expected surfaced GraphQL error, got %v", err)
	}
}

func TestViewerDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"usr_bot"}}}`))
	}))
	defer srv.Close()
	c := New("k")
	c.url = srv.URL
	id, err := c.Viewer(context.Background())
	if err != nil || id != "usr_bot" {
		t.Fatalf("Viewer = (%q,%v), want (usr_bot,nil)", id, err)
	}
}

func TestHasLabel(t *testing.T) {
	iss := &Issue{Labels: []Label{{Name: "Bug"}, {Name: "mando"}}}
	if !iss.HasLabel("mando") || !iss.HasLabel("MANDO") || iss.HasLabel("feature") {
		t.Error("HasLabel case-insensitive match wrong")
	}
}
