package control

import (
	"regexp"
	"testing"
)

var branchLegal = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"Add Changelog":           "add-changelog",
		"  fix   login/timeout  ": "fix-login-timeout",
		"**Refactor** Auth!!!":    "refactor-auth",
		"UPPER_snake.case":        "upper-snake-case",
		"":                        "",
		"---":                     "",
	}
	for in, want := range cases {
		if got := sanitizeSlug(in); got != want {
			t.Errorf("sanitizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDeterministicSlug(t *testing.T) {
	cases := map[string]string{
		"Add a CHANGELOG.md file to the repo": "add-changelog-md-file-repo",
		"Please fix the login timeout":        "fix-login-timeout",
		"!!!":                                 "task",
	}
	for in, want := range cases {
		if got := deterministicSlug(in); got != want {
			t.Errorf("deterministicSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// A composed branch (agent/<slug>-<suffix>) must always be a legal, readable git ref.
func TestBranchIsLegal(t *testing.T) {
	for _, prompt := range []string{
		"Add a CHANGELOG.md", "Fix the bug!!!", "   ", "wire up OAuth2 / PKCE flow",
	} {
		branch := "agent/" + deterministicSlug(prompt) + "-" + branchSuffix("s_DVPF9CTYPCQZ97FZTCK6DKJR76")
		ref := branch[len("agent/"):]
		if !branchLegal.MatchString(ref) {
			t.Errorf("prompt %q -> illegal branch ref %q", prompt, ref)
		}
	}
}
