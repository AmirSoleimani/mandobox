package control

import "testing"

func TestToSlackMrkdwn(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold**", "*bold*"},
		{"__bold__", "*bold*"},
		{"# Heading", "*Heading*"},
		{"### A Baby Story", "*A Baby Story*"},
		{"see [the docs](https://x.dev/y)", "see <https://x.dev/y|the docs>"},
		{"- one\n- two", "• one\n• two"},
		{"* item", "• item"},
		{"~~gone~~", "~gone~"},
		{"plain text", "plain text"},
		// code is protected: contents never rewritten
		{"`**not bold**`", "`**not bold**`"},
		{"```\n# not a heading\n**x**\n```", "```\n# not a heading\n**x**\n```"},
		{"use `x` and **y**", "use `x` and *y*"},
	}
	for _, c := range cases {
		if got := toSlackMrkdwn(c.in); got != c.want {
			t.Errorf("toSlackMrkdwn(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
