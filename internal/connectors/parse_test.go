package connectors

import "testing"

func TestParseSlash(t *testing.T) {
	cases := []struct {
		text, bot, wantCmd, wantRest string
		wantOK                       bool
	}{
		{"/mando owner/repo do it", "mybot", "mando", "owner/repo do it", true},
		{"/mando", "mybot", "mando", "", true},
		{"/mando@mybot owner/repo x", "mybot", "mando", "owner/repo x", true},
		{"/mando@MyBot owner/repo x", "mybot", "mando", "owner/repo x", true}, // case-insensitive @mention
		{"/mando@otherbot x", "mybot", "", "", false},                        // addressed to a different bot
		{"/start", "mybot", "start", "", true},
		{"/help", "mybot", "help", "", true},
		{"/start@mybot", "mybot", "start", "", true},
		{"/help@otherbot", "mybot", "", "", false}, // addressed to a different bot
		{"/foo bar baz", "mybot", "foo", "bar baz", true},
		{"just a chat message", "mybot", "", "", false},
	}
	for _, c := range cases {
		cmd, rest, ok := parseSlash(c.text, c.bot)
		if ok != c.wantOK || cmd != c.wantCmd || rest != c.wantRest {
			t.Errorf("parseSlash(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.text, c.bot, cmd, rest, ok, c.wantCmd, c.wantRest, c.wantOK)
		}
	}
}

func TestParseDispatch(t *testing.T) {
	cases := []struct {
		rest, wantRepo, wantPrompt string
		wantCheap, wantOK          bool
	}{
		{"owner/repo do the thing", "owner/repo", "do the thing", false, true},
		{"--cheap owner/repo fix the lint", "owner/repo", "fix the lint", true, true},
		{"noslug do it", "", "", false, false},
		{"owner/repo", "", "", false, false},
		{"", "", "", false, false},
		{"--cheap owner/repo", "", "", false, false},
	}
	for _, c := range cases {
		repo, prompt, cheap, ok := parseDispatch(c.rest)
		if ok != c.wantOK || repo != c.wantRepo || prompt != c.wantPrompt || cheap != c.wantCheap {
			t.Errorf("parseDispatch(%q) = (%q,%q,%v,%v), want (%q,%q,%v,%v)",
				c.rest, repo, prompt, cheap, ok, c.wantRepo, c.wantPrompt, c.wantCheap, c.wantOK)
		}
	}
}
