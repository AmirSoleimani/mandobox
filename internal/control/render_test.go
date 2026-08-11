package control

import (
	"os"
	"regexp"
	"testing"
)

// The workflow emits canonical chat markup in Slack's mrkdwn dialect. The Slack connector sends it
// as-is (byte-identical to production — nothing to test there); non-Slack connectors translate it.
// These tests pin the Telegram translation, including injection safety, and prove the emoji table
// covers every shortcode the workflow actually uses.

func TestTelegramRender(t *testing.T) {
	cases := []struct{ name, canonical, want string }{
		{
			"pr_opened_link_and_bold",
			":tada: *PR opened* <https://github.com/o/r/pull/5|#5>",
			`🎉 <b>PR opened</b> <a href="https://github.com/o/r/pull/5">#5</a>`,
		},
		{
			"code_protects_underscores_and_escapes_gt",
			":robot_face: *Task dispatched* `s_X`\n*repo* `o/r`\n> do it",
			"🤖 <b>Task dispatched</b> <code>s_X</code>\n<b>repo</b> <code>o/r</code>\n&gt; do it",
		},
		{
			"italic_footer_but_not_snake_case",
			":speech_balloon: touched `file_name.go` — _$0.0500 · 100 tokens_",
			"💬 touched <code>file_name.go</code> — <i>$0.0500 · 100 tokens</i>",
		},
		{
			"fence_body_escaped",
			":inbox_tray: *Detached.* left:\n```\nM <file>.go\n```\ntell me",
			"📥 <b>Detached.</b> left:\n<pre>M &lt;file&gt;.go\n</pre>\ntell me",
		},
		{
			"unsafe_link_scheme_dropped_to_label",
			"see <javascript:alert(1)|here>",
			"see here",
		},
		{
			"html_in_user_text_is_escaped_not_injected",
			"reply: <b>not real</b> & <script>x</script>",
			"reply: &lt;b&gt;not real&lt;/b&gt; &amp; &lt;script&gt;x&lt;/script&gt;",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canonicalToTelegramHTML(c.canonical); got != c.want {
				t.Errorf("mismatch:\n in:   %q\n got:  %q\n want: %q", c.canonical, got, c.want)
			}
		})
	}
}

// TestEmojiCoverage scans the actual source for the :shortcode: emoji the workflow emits and fails if
// any lacks a Unicode mapping — so a non-Slack connector never shows a raw shortcode. This is the real
// guard the render.go comment promises (not a hand-maintained list).
func TestEmojiCoverage(t *testing.T) {
	re := regexp.MustCompile(`:([a-z][a-z0-9_]+):`)
	for _, f := range []string{"workflow.go", "activities.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			if _, ok := emojiUnicode[m[1]]; !ok {
				t.Errorf("%s uses :%s: but emojiUnicode has no Unicode mapping for it", f, m[1])
			}
		}
	}
}
