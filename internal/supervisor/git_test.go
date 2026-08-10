package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRNumberFromURL(t *testing.T) {
	cases := map[string]int{
		"https://github.com/acme/foo/pull/42":  42,
		"https://github.com/acme/foo/pull/1/":  1,
		"not a url":                               0,
		"https://github.com/acme/foo/pull/abc": 0,
	}
	for url, want := range cases {
		if got := prNumberFromURL(url); got != want {
			t.Errorf("prNumberFromURL(%q) = %d, want %d", url, got, want)
		}
	}
}

func TestSetupCredentialsNoTokenInURLOrEnv(t *testing.T) {
	runDir := t.TempDir()
	fleetDir := t.TempDir()
	cfg := mustCfg(t, validMMDS)
	g := NewGit(newFakeRunner(), cfg, filepath.Join(t.TempDir(), "repo"), fleetDir)
	g.tokenPath = filepath.Join(runDir, "token")

	if err := g.SetupCredentials(context.Background()); err != nil {
		t.Fatalf("SetupCredentials: %v", err)
	}
	// Token lands only in the tmpfs file, at 0600.
	info, err := os.Stat(g.tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token mode = %o, want 600", info.Mode().Perm())
	}
	// The helper reads the token from disk — it is not baked into the script.
	helper, err := os.ReadFile(filepath.Join(fleetDir, "git-credential-helper.sh"))
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if strings.Contains(string(helper), cfg.GitHub.Token) {
		t.Error("token must not be embedded in the credential helper")
	}
	if !strings.Contains(string(helper), g.tokenPath) {
		t.Error("helper should cat the token path")
	}
}

func TestCommitNoChanges(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs["status --porcelain"] = "   \n"
	g := NewGit(fr, mustCfg(t, validMMDS), t.TempDir(), t.TempDir())

	sha, changed, err := g.Commit(context.Background(), "msg")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if changed || sha != "" {
		t.Fatalf("Commit reported changed=%v sha=%q, want no changes", changed, sha)
	}
	if fr.ran("commit -m") {
		t.Error("should not commit when there are no changes")
	}
}
