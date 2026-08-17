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

	// No allowlist → free-form inference. A full owner/repo slug passes through.
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"acme/widgets"}]}`))
	}))
	defer full.Close()
	c = &linearConnector{}
	c.llm = llm.New(full.URL, "t", "m")
	if r, ok := c.resolveRepo(ctx, &linear.Issue{Title: "x"}); !ok || r != "acme/widgets" {
		t.Fatalf("free-form full = (%q,%v), want acme/widgets", r, ok)
	}

	// No allowlist: a bare repo name is not a slug → ask (issues must name owner/repo, never guess).
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"Dashboard"}]}`))
	}))
	defer bare.Close()
	c = &linearConnector{}
	c.llm = llm.New(bare.URL, "t", "m")
	if r, ok := c.resolveRepo(ctx, &linear.Issue{Title: "x"}); ok {
		t.Fatalf("bare name must be ok=false, got (%q,%v)", r, ok)
	}

	// No allowlist: UNRESOLVED → ask.
	c = &linearConnector{}
	c.llm = llm.New(unres.URL, "t", "m")
	if r, ok := c.resolveRepo(ctx, &linear.Issue{Title: "x"}); ok {
		t.Fatalf("free-form unresolved must be ok=false, got (%q,%v)", r, ok)
	}
}

func TestNormalizeRepoAnswer(t *testing.T) {
	cases := []struct {
		ans, want string
		ok        bool
	}{
		{"acme/widgets", "acme/widgets", true},
		{"`acme/widgets`", "acme/widgets", true}, // decoration stripped
		{"dashboard", "", false},                 // bare name → not a slug → miss (issues name owner/repo)
		{"UNRESOLVED", "", false},
		{"unresolved", "", false},
		{"", "", false},
		{"a/b/c", "", false},         // too many segments
		{"acme/", "", false},         // empty repo
		{"/widgets", "", false},      // empty owner
		{"acme/wid gets", "", false}, // illegal char
	}
	for _, c := range cases {
		got, ok := normalizeRepoAnswer(c.ans)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeRepoAnswer(%q) = (%q,%v), want (%q,%v)", c.ans, got, ok, c.want, c.ok)
		}
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
