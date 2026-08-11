package main

import "testing"

func TestParseMandoCommand(t *testing.T) {
	cases := []struct {
		text, bot, wantRest string
		wantOK              bool
	}{
		{"/mando owner/repo do it", "mybot", "owner/repo do it", true},
		{"/mando", "mybot", "", true},
		{"/mando@mybot owner/repo x", "mybot", "owner/repo x", true},
		{"/mando@MyBot owner/repo x", "mybot", "owner/repo x", true}, // case-insensitive @mention
		{"/mando@otherbot x", "mybot", "", false},                    // addressed to a different bot
		{"/start", "mybot", "", false},
		{"just a chat message", "mybot", "", false},
		{"/mandolin not a command", "mybot", "", false},
	}
	for _, c := range cases {
		rest, ok := parseMandoCommand(c.text, c.bot)
		if ok != c.wantOK || rest != c.wantRest {
			t.Errorf("parseMandoCommand(%q,%q) = (%q,%v), want (%q,%v)", c.text, c.bot, rest, ok, c.wantRest, c.wantOK)
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
		{"noslug do it", "", "", false, false}, // repo must contain a slash
		{"owner/repo", "", "", false, false},   // missing prompt
		{"", "", "", false, false},
		{"--cheap owner/repo", "", "", false, false}, // cheap but no prompt
	}
	for _, c := range cases {
		repo, prompt, cheap, ok := parseDispatch(c.rest)
		if ok != c.wantOK || repo != c.wantRepo || prompt != c.wantPrompt || cheap != c.wantCheap {
			t.Errorf("parseDispatch(%q) = (%q,%q,%v,%v), want (%q,%q,%v,%v)",
				c.rest, repo, prompt, cheap, ok, c.wantRepo, c.wantPrompt, c.wantCheap, c.wantOK)
		}
	}
}
