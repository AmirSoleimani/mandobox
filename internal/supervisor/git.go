package supervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// lastLine returns the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// prNumberFromURL extracts the trailing PR number from a GitHub PR URL, or 0.
func prNumberFromURL(url string) int {
	url = strings.TrimRight(url, "/")
	idx := strings.LastIndex(url, "/")
	if idx < 0 {
		return 0
	}
	n, err := strconv.Atoi(url[idx+1:])
	if err != nil {
		return 0
	}
	return n
}

// defaultTokenPath is the tmpfs file the git credential helper reads. Keeping the token here
// (not in the remote URL or an environment variable) is what lets a refresh update every
// subsequent git operation with no process restart.
const defaultTokenPath = "/run/gh/token"

// Git drives the repository operations the supervisor performs (the agent edits files; the
// supervisor commits, pushes, and opens the PR).
type Git struct {
	runner    Runner
	cfg       BootConfig
	repoDir   string
	fleetDir  string // /workspace/.fleet — helper script + bookkeeping
	tokenPath string // credential-helper token file (overridable in tests)
}

// NewGit returns a Git for repoDir (the checked-out tree) with fleetDir for helpers.
func NewGit(runner Runner, cfg BootConfig, repoDir, fleetDir string) *Git {
	return &Git{runner: runner, cfg: cfg, repoDir: repoDir, fleetDir: fleetDir, tokenPath: defaultTokenPath}
}

// helperScript is the script git invokes for every operation. It reads the current token
// from disk, so rotating the token is a file write.
func (g *Git) helperScript() string {
	return "#!/bin/sh\n" +
		`[ "$1" = "get" ] || exit 0` + "\n" +
		`echo "username=x-access-token"` + "\n" +
		`echo "password=$(cat ` + g.tokenPath + `)"` + "\n"
}

// SetupCredentials installs the credential helper and the bot identity. The token is never
// placed in the clone URL or an env var.
func (g *Git) SetupCredentials(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(g.tokenPath), 0o700); err != nil {
		return fmt.Errorf("git creds: mkdir token dir: %w", err)
	}
	if err := os.WriteFile(g.tokenPath, []byte(g.cfg.GitHub.Token), 0o600); err != nil {
		return fmt.Errorf("git creds: write token: %w", err)
	}
	if err := os.MkdirAll(g.fleetDir, 0o700); err != nil {
		return fmt.Errorf("git creds: mkdir fleet dir: %w", err)
	}
	helper := filepath.Join(g.fleetDir, "git-credential-helper.sh")
	if err := os.WriteFile(helper, []byte(g.helperScript()), 0o700); err != nil {
		return fmt.Errorf("git creds: write helper: %w", err)
	}
	name := g.cfg.GitHub.BotUser
	if name == "" {
		name = "mando-agent[bot]"
	}
	email := g.cfg.GitHub.BotEmail
	if email == "" {
		email = "mando-agent[bot]@users.noreply.github.com"
	}
	for _, kv := range [][2]string{
		// Host-scoped helper: git invokes it ONLY for github.com, so the installation token is never
		// offered to a third-party remote (e.g. a submodule on gitlab.com/bitbucket.org).
		{"credential.https://github.com.helper", "!" + helper},
		{"user.name", name},
		{"user.email", email},
		{"safe.directory", g.repoDir},
	} {
		if err := g.runner.Run(ctx, "git", "config", "--global", kv[0], kv[1]); err != nil {
			return fmt.Errorf("git config %s: %w", kv[0], err)
		}
	}
	return nil
}

// RefreshToken rewrites the token file; subsequent git operations pick it up with no
// restart. Used by the T-10min refresh path.
func (g *Git) RefreshToken(token string) error {
	return os.WriteFile(g.tokenPath, []byte(token), 0o600)
}

// Prepare gets the repo onto the agent branch: clone + branch on the initial run, or
// fetch + checkout on a resume (the workspace, and thus the clone, persists).
func (g *Git) Prepare(ctx context.Context) error {
	branch := g.cfg.Branch()
	if _, err := os.Stat(filepath.Join(g.repoDir, ".git")); err == nil {
		// Resume: existing clone on the workspace.
		if err := g.git(ctx, "fetch", "origin"); err != nil {
			return err
		}
		if err := g.git(ctx, "checkout", branch); err != nil {
			return err
		}
		g.excludeCaptureDir()
		return nil
	}
	// Initial: fresh clone of the base branch, then a new agent branch.
	if err := g.runner.Run(ctx, "git", "clone", "--branch", g.cfg.Repo.BaseBranch,
		g.cfg.Repo.CloneURL, g.repoDir); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	if err := g.git(ctx, "checkout", "-b", branch); err != nil {
		return err
	}
	g.excludeCaptureDir()
	return nil
}

// excludeCaptureDir keeps the agent's screenshot directory (.mando/) out of commits via the repo-local
// .git/info/exclude — which is NOT itself committed. So a capture never lands in the tree, and a
// screenshot-only request stays a true no-op (no PR) without the agent having to edit .gitignore.
// Best-effort: on any failure the agent's own artifact-hygiene still applies.
func (g *Git) excludeCaptureDir() {
	p := filepath.Join(g.repoDir, ".git", "info", "exclude")
	if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), "\n.mando/") {
		return // already excluded (persisted workspace)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n# mandobox visual-verification captures — never committed\n.mando/\n")
}

// PendingDiff stages all changes and returns a --stat summary plus the unified diff of what a
// commit would capture, so a message can be written from the real change. changed=false when the
// tree is clean (the no-op turn). Staging here is harmless: Commit re-stages idempotently.
func (g *Git) PendingDiff(ctx context.Context) (summary, patch string, changed bool, err error) {
	status, err := g.output(ctx, "status", "--porcelain")
	if err != nil {
		return "", "", false, err
	}
	if strings.TrimSpace(status) == "" {
		return "", "", false, nil
	}
	if err := g.git(ctx, "add", "-A"); err != nil {
		return "", "", false, err
	}
	if summary, err = g.output(ctx, "diff", "--cached", "--stat"); err != nil {
		return "", "", false, err
	}
	if patch, err = g.output(ctx, "diff", "--cached"); err != nil {
		return "", "", false, err
	}
	return summary, patch, true, nil
}

// Commit stages and commits all changes. It reports changed=false when the agent produced
// no diff — a legitimate outcome that must not look like a failure.
func (g *Git) Commit(ctx context.Context, message string) (sha string, changed bool, err error) {
	status, err := g.output(ctx, "status", "--porcelain")
	if err != nil {
		return "", false, err
	}
	if strings.TrimSpace(status) == "" {
		return "", false, nil
	}
	if err := g.git(ctx, "add", "-A"); err != nil {
		return "", false, err
	}
	if err := g.git(ctx, "commit", "-m", message); err != nil {
		return "", false, fmt.Errorf("commit: %w", err)
	}
	sha, err = g.output(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", true, err
	}
	return strings.TrimSpace(sha), true, nil
}

// Discard resets the working tree to HEAD — revert tracked edits (staged AND unstaged) and remove new
// untracked files — so a plan turn is a guaranteed codebase no-op (plan mode explores and writes a plan;
// it must not change code). `reset --hard` (not `checkout -- .`) so even staged edits can't survive into a
// later build turn. The .mando/ capture dir is git-excluded (excludeCaptureDir), so `git clean -fd` leaves
// it and the just-harvested sentinel alone; only stray source edits and new files are undone.
func (g *Git) Discard(ctx context.Context) error {
	if err := g.git(ctx, "reset", "--hard", "HEAD"); err != nil {
		return err
	}
	return g.git(ctx, "clean", "-fd")
}

// Push publishes the agent branch. Agents can only push to agent/* — main is protected by
// GitHub, not by us.
func (g *Git) Push(ctx context.Context) error {
	return g.git(ctx, "push", "-u", "origin", g.cfg.Branch())
}

// OpenPR creates the pull request via gh, passing the token only in this invocation's
// environment. Returns the PR number and URL.
func (g *Git) OpenPR(ctx context.Context, title, body string) (int, string, error) {
	out, err := g.runner.OutputEnv(ctx, g.ghEnv(), "gh", "pr", "create",
		"--repo", g.cfg.Repo.Slug,
		"--base", g.cfg.Repo.BaseBranch,
		"--head", g.cfg.Branch(),
		"--title", title,
		"--body", body,
	)
	if err != nil {
		return 0, "", fmt.Errorf("gh pr create: %w", err)
	}
	url := lastLine(out)
	return prNumberFromURL(url), url, nil
}

func (g *Git) ghEnv() []string { return []string{"GH_TOKEN=" + g.cfg.GitHub.Token} }

// git runs a git subcommand in the repo dir.
func (g *Git) git(ctx context.Context, args ...string) error {
	return g.runner.Run(ctx, "git", append([]string{"-C", g.repoDir}, args...)...)
}

func (g *Git) output(ctx context.Context, args ...string) (string, error) {
	return g.runner.Output(ctx, "git", append([]string{"-C", g.repoDir}, args...)...)
}
