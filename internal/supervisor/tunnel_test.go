package supervisor

import "testing"

// tunnelName must always yield a VS Code-legal tunnel name (2–20 chars, lowercase alphanumeric),
// or the URL built from it won't resolve.
func TestTunnelNameIsValid(t *testing.T) {
	for _, sid := range []string{
		"s_6PJXFAGAE4SNFE0E5WCPCFYGJA",
		"s_0123456789ABCDEFGHJKMNPQRS",
		"S_A",
		"x",
		"",
	} {
		n := tunnelName(sid)
		if len(n) < 2 || len(n) > 20 {
			t.Errorf("%q -> %q: length %d out of [2,20]", sid, n, len(n))
		}
		for _, r := range n {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
				t.Errorf("%q -> %q: illegal char %q", sid, n, r)
			}
		}
	}
}

// tunnelURLField must isolate the bare vscode.dev URL from code tunnel's decorated output line,
// dropping trailing whitespace, ANSI resets, and sentence punctuation — the link we post must be
// clickable verbatim.
func TestTunnelURLField(t *testing.T) {
	want := "https://vscode.dev/tunnel/mando123/workspace/repo"
	for _, in := range []string{
		want,
		want + "   ",
		want + "\x1b[0m",
		want + ".",
		want + " and open it",
	} {
		if got := tunnelURLField(in); got != want {
			t.Errorf("tunnelURLField(%q) = %q, want %q", in, got, want)
		}
	}
}
