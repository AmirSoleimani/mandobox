package connectors

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AmirSoleimani/mandobox/internal/linear"
	"github.com/AmirSoleimani/mandobox/internal/llm"
)

func TestSplitRepos(t *testing.T) {
	got := splitRepos("a/b, c/d\n e/f\t")
	if len(got) != 3 || got[0] != "a/b" || got[1] != "c/d" || got[2] != "e/f" {
		t.Errorf("splitRepos = %v", got)
	}
	if len(splitRepos("   ")) != 0 {
		t.Error("blank input → empty")
	}
}

func TestIsTodoState(t *testing.T) {
	for _, s := range []string{"triage", "backlog", "unstarted"} {
		if !isTodoState(s) {
			t.Errorf("%q should be a to-do state", s)
		}
	}
	for _, s := range []string{"started", "completed", "canceled", ""} {
		if isTodoState(s) {
			t.Errorf("%q should NOT be a to-do state", s)
		}
	}
}

func TestLRUSet(t *testing.T) {
	s := newLRUSet(2)
	if s.seen("a") {
		t.Error("first 'a' should be new")
	}
	if !s.seen("a") {
		t.Error("second 'a' should be a duplicate")
	}
	s.seen("b")
	s.seen("c") // evicts "a" (cap 2)
	if s.seen("a") {
		t.Error("'a' was evicted → should read as new again")
	}
}

func TestResolveRepo(t *testing.T) {
	ctx := context.Background()

	// Single-repo allowlist short-circuits (no LLM call).
	c := &linearConnector{allowlist: []string{"org/only"}}
	if r, ok := c.resolveRepo(ctx, &linear.Issue{}); !ok || r != "org/only" {
		t.Fatalf("single allowlist = (%q,%v)", r, ok)
	}

	// No allowlist but a default is set → the default.
	c = &linearConnector{defaultRepo: "org/def"}
	if r, ok := c.resolveRepo(ctx, &linear.Issue{}); !ok || r != "org/def" {
		t.Fatalf("default = (%q,%v)", r, ok)
	}

	// Multi-allowlist: the LLM answer is validated against the list and the CANONICAL form is returned.
	pick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Org/Repo-Two"}]}`))
	}))
	defer pick.Close()
	c = &linearConnector{allowlist: []string{"Org/Repo-One", "Org/Repo-Two"}}
	c.llm = llm.New(pick.URL, "t", "m")
	if r, ok := c.resolveRepo(ctx, &linear.Issue{Title: "x"}); !ok || r != "Org/Repo-Two" {
		t.Fatalf("multi allowlist = (%q,%v), want Org/Repo-Two", r, ok)
	}

	// Unresolved (not in the list) with no default → ("", false); we ask, never guess.
	unres := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"UNRESOLVED"}]}`))
	}))
	defer unres.Close()
	c = &linearConnector{allowlist: []string{"Org/Repo-One", "Org/Repo-Two"}}
	c.llm = llm.New(unres.URL, "t", "m")
	if r, ok := c.resolveRepo(ctx, &linear.Issue{Title: "x"}); ok {
		t.Fatalf("unresolved must be ok=false, got (%q,%v)", r, ok)
	}
}

func TestAlreadyAsked(t *testing.T) {
	c := &linearConnector{viewerID: "bot"}
	if !c.alreadyAsked(&linear.Issue{Comments: []linear.Comment{{UserID: "human"}, {UserID: "bot"}}}) {
		t.Error("newest comment ours → alreadyAsked")
	}
	if c.alreadyAsked(&linear.Issue{Comments: []linear.Comment{{UserID: "bot"}, {UserID: "human"}}}) {
		t.Error("newest comment human → not alreadyAsked")
	}
	if c.alreadyAsked(&linear.Issue{}) {
		t.Error("no comments → not alreadyAsked")
	}
}
