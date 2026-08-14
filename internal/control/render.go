package control

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

// The workflow emits canonical chat markup in Slack's mrkdwn dialect (*bold*, _italic_, ~strike~,
// <url|label>, `code`, ```fences```, :emoji: shortcodes) — the reference dialect chosen as the lingua
// franca. The Slack connector sends it as-is; every other connector translates it. This file holds the
// Telegram (HTML) translation and the shared emoji table. A new connector adds one such translator —
// the workflow never changes.

// emojiUnicode maps the :shortcode: emoji the workflow emits to Unicode, for connectors that (unlike
// Slack) don't render shortcodes natively. TestEmojiCoverage scans the source and fails if the workflow
// uses a shortcode missing here, so this stays complete.
var emojiUnicode = map[string]string{
	"robot_face": "🤖", "tada": "🎉", "inbox_tray": "📥", "outbox_tray": "📤",
	"speech_balloon": "💬", "left_speech_bubble": "🗨️", "grey_question": "❔",
	"checkered_flag": "🏁", "x": "❌", "no_entry": "⛔", "zzz": "💤",
	"arrow_up": "⬆️", "arrows_counterclockwise": "🔄", "mag": "🔍",
	"information_source": "ℹ️", "globe_with_meridians": "🌐",
	"cyclone": "🌀", "sparkles": "✨", "thought_balloon": "💭", "crystal_ball": "🔮",
	"gear": "⚙️", "brain": "🧠", "ocean": "🌊", "hammer_and_wrench": "🛠️",
	"clipboard": "📋", "white_check_mark": "✅", "memo": "📝",
}

var emojiRe = regexp.MustCompile(`:([a-z0-9_+-]+):`)

// replaceEmoji swaps :shortcode: for its Unicode; unknown shortcodes are left untouched.
func replaceEmoji(s string) string {
	return emojiRe.ReplaceAllStringFunc(s, func(m string) string {
		if u, ok := emojiUnicode[strings.Trim(m, ":")]; ok {
			return u
		}
		return m
	})
}

var (
	tgFence  = regexp.MustCompile("(?s)```(?:[a-zA-Z0-9_+-]*\n)?(.*?)```")
	tgInline = regexp.MustCompile("`([^`\n]+)`")
	tgLink   = regexp.MustCompile(`<([^|>\n]+)\|([^>\n]+)>`) // Slack <url|label>
	tgBold   = regexp.MustCompile(`\*([^*\n]+)\*`)           // Slack *bold*
	tgStrike = regexp.MustCompile(`~([^~\n]+)~`)             // Slack ~strike~
	// Slack _italic_, flanked by non-word (and non-'/') so snake_case and path/_x_ aren't mangled.
	tgItalic       = regexp.MustCompile(`(^|[^\w/])_([^_\n]+?)_([^\w/]|$)`)
	tgAllowedScheme = regexp.MustCompile(`(?i)^(https?|tg|mailto):`)
)

// canonicalToTelegramHTML translates the canonical Slack-mrkdwn markup into the HTML subset the
// Telegram Bot API accepts (parse_mode=HTML): <b>, <i>, <s>, <a>, <code>, <pre>. Like the Slack side it
// is a best-effort rewriter, not a full parser. It protects code spans/fences and Slack links (which
// contain < and >) BEFORE HTML-escaping the rest, so user content can't inject markup, disallowed URL
// schemes are dropped, and formatting rewrites never touch link URLs or code bodies.
func canonicalToTelegramHTML(s string) string {
	if s == "" {
		return s
	}
	type span struct{ a, b string }

	// 1. Pull code spans/fences out (fences before inline) so their contents survive everything below.
	var codes []span
	protect := func(re *regexp.Regexp, tag string) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			codes = append(codes, span{tag, re.FindStringSubmatch(m)[1]})
			return "\x00c" + strconv.Itoa(len(codes)-1) + "\x00"
		})
	}
	protect(tgFence, "pre")
	protect(tgInline, "code")

	// 2. Pull Slack links out before HTML-escaping (they carry < and >). {a: url, b: label}.
	var links []span
	s = tgLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := tgLink.FindStringSubmatch(m)
		links = append(links, span{sub[1], sub[2]})
		return "\x00l" + strconv.Itoa(len(links)-1) + "\x00"
	})

	// 3. Emoji, then HTML-escape ordinary text (placeholders carry no HTML-special bytes).
	s = replaceEmoji(s)
	s = html.EscapeString(s)

	// 4. Inline formatting on the escaped, code-and-link-free text.
	s = tgBold.ReplaceAllString(s, "<b>$1</b>")
	s = tgStrike.ReplaceAllString(s, "<s>$1</s>")
	s = tgItalic.ReplaceAllString(s, "$1<i>$2</i>$3")

	// 5. Restore links — dropping any non-http(s)/tg/mailto scheme to the plain label.
	for i, l := range links {
		var repl string
		if tgAllowedScheme.MatchString(l.a) {
			repl = `<a href="` + html.EscapeString(l.a) + `">` + html.EscapeString(l.b) + `</a>`
		} else {
			repl = html.EscapeString(l.b)
		}
		s = strings.Replace(s, "\x00l"+strconv.Itoa(i)+"\x00", repl, 1)
	}
	// 6. Restore code with HTML-escaped bodies.
	for i, c := range codes {
		s = strings.Replace(s, "\x00c"+strconv.Itoa(i)+"\x00",
			"<"+c.a+">"+html.EscapeString(c.b)+"</"+c.a+">", 1)
	}
	return s
}

// canonicalToLinearMarkdown translates canonical Slack-mrkdwn into GitHub-flavored Markdown, which Linear
// renders in comments: *bold*→**bold** (single * is italic in GFM, so bold MUST double), _italic_→*italic*,
// ~strike~→~~strike~~, <url|label>→[label](url), `code`/```fences``` pass through, :emoji:→Unicode. Like
// the Telegram translator it's a best-effort rewriter (protect code+links, rewrite emphasis, restore) — but
// the target is Markdown, so there's no HTML escaping.
func canonicalToLinearMarkdown(s string) string {
	if s == "" {
		return s
	}
	type span struct{ tag, body string }

	// 1. Protect code spans/fences so their bodies survive the emphasis rewrites (fences before inline).
	var codes []span
	protect := func(re *regexp.Regexp, tag string) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			codes = append(codes, span{tag, re.FindStringSubmatch(m)[1]})
			return "\x00c" + strconv.Itoa(len(codes)-1) + "\x00"
		})
	}
	protect(tgFence, "fence")
	protect(tgInline, "inline")

	// 2. Protect Slack links (they carry < >); rewritten to [label](url) on restore.
	var links []span // tag=url, body=label
	s = tgLink.ReplaceAllStringFunc(s, func(m string) string {
		sub := tgLink.FindStringSubmatch(m)
		links = append(links, span{sub[1], sub[2]})
		return "\x00l" + strconv.Itoa(len(links)-1) + "\x00"
	})

	// 3. Emoji + emphasis rewrites (on link-and-code-free text).
	s = replaceEmoji(s)
	s = tgBold.ReplaceAllString(s, "**$1**")
	s = tgStrike.ReplaceAllString(s, "~~$1~~")
	s = tgItalic.ReplaceAllString(s, "$1*$2*")

	// 4. Restore links — dropping any non-http(s)/mailto scheme to the plain label.
	for i, l := range links {
		var repl string
		if tgAllowedScheme.MatchString(l.tag) {
			repl = "[" + l.body + "](" + l.tag + ")"
		} else {
			repl = l.body
		}
		s = strings.Replace(s, "\x00l"+strconv.Itoa(i)+"\x00", repl, 1)
	}
	// 5. Restore code/fences.
	for i, c := range codes {
		if c.tag == "fence" {
			s = strings.Replace(s, "\x00c"+strconv.Itoa(i)+"\x00", "```\n"+c.body+"\n```", 1)
		} else {
			s = strings.Replace(s, "\x00c"+strconv.Itoa(i)+"\x00", "`"+c.body+"`", 1)
		}
	}
	return s
}
