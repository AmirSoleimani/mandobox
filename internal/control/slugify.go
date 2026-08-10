package control

import (
	"context"
	"regexp"
	"strings"
)

var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// slugStop are filler words dropped from the branch slug so it stays meaningful.
var slugStop = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "and": true,
	"in": true, "on": true, "with": true, "please": true, "that": true, "this": true, "into": true,
	"your": true, "my": true, "it": true, "its": true, "is": true, "be": true, "so": true,
}

// SlugifyTask turns a task prompt into a short, meaningful, git-legal branch slug from its first few
// meaningful words — e.g. "Add a CHANGELOG.md file" → "add-changelog-md-file". It's an activity (the
// branch is chosen once, at dispatch, and recorded) but deterministic: the cheap model was tried here
// and produced rambly, duplicated slugs, so a predictable word-based slug is used instead.
func (a *Activities) SlugifyTask(_ context.Context, prompt string) (string, error) {
	return deterministicSlug(prompt), nil
}

// sanitizeSlug forces an arbitrary string into a git-legal, readable branch slug: lowercase,
// alphanumeric words joined by single hyphens, capped in length.
func sanitizeSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 { // trim to a word boundary so we never leave a half-word tail
		s = s[:40]
		if i := strings.LastIndexByte(s, '-'); i > 0 {
			s = s[:i]
		}
	}
	return strings.Trim(s, "-")
}

// deterministicSlug builds a slug from the first few meaningful words of the prompt.
func deterministicSlug(prompt string) string {
	kept := make([]string, 0, 5)
	for _, w := range strings.Fields(strings.ToLower(prompt)) {
		w = strings.Trim(slugNonAlnum.ReplaceAllString(w, "-"), "-")
		if w == "" || slugStop[w] {
			continue
		}
		kept = append(kept, w)
		if len(kept) == 4 {
			break
		}
	}
	if len(kept) == 0 {
		return "task"
	}
	return sanitizeSlug(strings.Join(kept, "-"))
}

// branchSuffix is a short unique tail (from the session id) appended to the slug so two similarly
// named tasks never collide on the same branch.
func branchSuffix(sessionID string) string {
	s := slugNonAlnum.ReplaceAllString(strings.ToLower(sessionID), "")
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return s
}
