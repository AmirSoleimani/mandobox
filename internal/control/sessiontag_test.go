package control

import "testing"

// The session tag disambiguates several sessions sharing one flat chat (Telegram). It must be empty for
// threaded connectors (Slack) so their golden-tested output stays byte-identical, and the title must be
// single-line and emphasis-safe so it can't break the surrounding markup.

func TestTaskTitle(t *testing.T) {
	cases := []struct{ name, prompt, want string }{
		{"plain", "add a /healthz endpoint", "add a /healthz endpoint"},
		{"first_line_only", "make the header sticky\nand give it a shadow", "make the header sticky"},
		{"crlf", "fix the bug\r\nsecond line", "fix the bug"},
		{"strips_emphasis", "make the *header* _sticky_ and `bold`", "make the header sticky and bold"},
		{"trims_space", "   trim me   ", "trim me"},
		{"truncates", "0123456789012345678901234567890123456789012345678901234567890123456789", "012345678901234567890123456789012345678901234567890123456789…"},
	}
	for _, c := range cases {
		if got := taskTitle(c.prompt); got != c.want {
			t.Errorf("%s: taskTitle(%q) = %q, want %q", c.name, c.prompt, got, c.want)
		}
	}
}

func TestShortSID(t *testing.T) {
	if got := shortSID("s_6SF4YHEDMR0XG28RE6SSZNFXJH"); got != "s_6SF4YH" {
		t.Errorf("shortSID long = %q, want s_6SF4YH", got)
	}
	if got := shortSID("s_abc"); got != "s_abc" {
		t.Errorf("shortSID short = %q, want s_abc", got)
	}
}

func TestSessionTag(t *testing.T) {
	st := &State{SessionID: "s_6SF4YHEDMR0XG28RE6SSZNFXJH", Title: "add a /healthz endpoint"}

	// Threaded connector (Slack, the zero value): no tag — output must stay byte-identical.
	if got := sessionTag(st); got != "" {
		t.Errorf("sessionTag not flat = %q, want empty", got)
	}
	if got := sessionTagPlain(st); got != "" {
		t.Errorf("sessionTagPlain not flat = %q, want empty", got)
	}

	// Flat connector (Telegram): compact mrkdwn tag, and a plain variant for verbatim captions.
	st.Conversation.Flat = true
	if got, want := sessionTag(st), "`s_6SF4YH` _add a /healthz endpoint_"; got != want {
		t.Errorf("sessionTag flat = %q, want %q", got, want)
	}
	if got, want := sessionTagPlain(st), "s_6SF4YH · add a /healthz endpoint"; got != want {
		t.Errorf("sessionTagPlain flat = %q, want %q", got, want)
	}

	// No title (unlikely, but the tag must still identify the session by id alone).
	st.Title = ""
	if got, want := sessionTag(st), "`s_6SF4YH`"; got != want {
		t.Errorf("sessionTag flat no-title = %q, want %q", got, want)
	}
}
