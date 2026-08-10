package control

import (
	"regexp"
	"strconv"
	"strings"
)

// Slack renders "mrkdwn", not the GitHub-flavored Markdown the agent emits — so `**bold**`,
// `# headings`, and `[text](url)` show up as raw punctuation in a thread. toSlackMrkdwn rewrites the
// constructs agents actually produce into their mrkdwn equivalents. It is deliberately not a full
// Markdown parser: it protects code spans/fences (so their contents are never rewritten) and leaves
// everything it doesn't recognize untouched.
var (
	mdFence   = regexp.MustCompile("(?s)```.*?```")
	mdInline  = regexp.MustCompile("`[^`\n]+`")
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(\s*([^)\s]+)[^)]*\)`)
	mdHeading = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdBoldU   = regexp.MustCompile(`__([^_\n]+)__`)
	mdStrike  = regexp.MustCompile(`~~([^~\n]+)~~`)
	mdBullet  = regexp.MustCompile(`(?m)^([ \t]*)[-*+][ \t]+`)
)

func toSlackMrkdwn(s string) string {
	if s == "" {
		return s
	}
	// Swap code spans out for placeholders so no rewrite touches their contents (fences first, then
	// inline). The placeholder \x00N\x00 can't match any of the rules below.
	var code []string
	protect := func(re *regexp.Regexp) {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			code = append(code, m)
			return "\x00" + strconv.Itoa(len(code)-1) + "\x00"
		})
	}
	protect(mdFence)
	protect(mdInline)

	s = mdLink.ReplaceAllString(s, "<$2|$1>") // [text](url) -> <url|text>
	s = mdHeading.ReplaceAllString(s, "*$1*") // # Heading   -> *Heading* (mrkdwn has no headings)
	s = mdBold.ReplaceAllString(s, "*$1*")    // **bold**    -> *bold*
	s = mdBoldU.ReplaceAllString(s, "*$1*")   // __bold__    -> *bold*
	s = mdStrike.ReplaceAllString(s, "~$1~")  // ~~strike~~  -> ~strike~
	s = mdBullet.ReplaceAllString(s, "$1• ")  // - item      -> • item

	for i, c := range code {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", c, 1)
	}
	return s
}
