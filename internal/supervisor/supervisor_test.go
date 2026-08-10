package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestSupervisor(t *testing.T, cfg BootConfig) (*Supervisor, *fakeTransport, *fakeRunner, *fakeAgent) {
	t.Helper()
	ws := t.TempDir()
	home := t.TempDir()
	runDir := t.TempDir()
	fr := newFakeRunner()
	ft := newFakeTransport()
	fa := &fakeAgent{result: Result{
		SessionID: "claude-uuid", TotalCostUSD: 0.05,
		Usage: Usage{InputTokens: 100, OutputTokens: 50}, Result: "did the thing",
	}}
	sup := New(cfg, Deps{Bus: NewBus(ft, cfg.SessionID), Runner: fr, Agent: fa, Platform: &fakePlatform{}}, ws)
	sup.home = home
	sup.git.tokenPath = filepath.Join(runDir, "token")
	// Park almost immediately after the turn so Run() returns without a live control plane.
	sup.keepAlive = 5 * time.Millisecond
	// No gateway in unit tests: fall back to the static commit message instead of a real HTTP call.
	sup.commitMsg = func(context.Context, string, string, string, string) string { return "" }
	return sup, ft, fr, fa
}

func mustCfg(t *testing.T, body string) BootConfig {
	t.Helper()
	c, err := ParseBootConfig([]byte(body))
	if err != nil {
		t.Fatalf("ParseBootConfig: %v", err)
	}
	return c
}

// singleEvent returns the single turn event, ignoring the trailing session_idle the keep-alive
// loop publishes when it parks.
func singleEvent(t *testing.T, ft *fakeTransport) Event {
	t.Helper()
	var turns []Event
	for _, ev := range ft.events() {
		if ev.Type != EventSessionIdle {
			turns = append(turns, ev)
		}
	}
	if len(turns) != 1 {
		t.Fatalf("want exactly 1 turn event, got %d: %+v", len(turns), turns)
	}
	return turns[0]
}

func TestRunInitialOpensPR(t *testing.T) {
	sup, ft, fr, fa := newTestSupervisor(t, mustCfg(t, validMMDS))
	fr.outputs["status --porcelain"] = " M main.go\n"
	fr.outputs["rev-parse HEAD"] = "abc1234\n"
	fr.outputs["pr create"] = "https://github.com/chelodo/foo/pull/42\n"

	if err := sup.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := singleEvent(t, ft)
	if ev.Type != EventPROpened || ev.PRNumber != 42 || !strings.HasSuffix(ev.PRURL, "/42") {
		t.Fatalf("event = %+v, want pr_opened #42", ev)
	}
	if ev.CostUSD != 0.05 || ev.Tokens != 150 {
		t.Errorf("cost/tokens = %v/%d, want 0.05/150", ev.CostUSD, ev.Tokens)
	}
	if !fr.ran("git clone") || !fr.ran("push -u origin agent/") || !fr.ran("gh pr create") {
		t.Error("expected clone, push, and gh pr create")
	}
	if fa.gotSpec.Resume {
		t.Error("initial run should not be a resume")
	}
	// ~/.claude symlink into the workspace (the load-bearing line).
	if target, err := os.Readlink(filepath.Join(sup.home, ".claude")); err != nil ||
		target != filepath.Join(sup.workspaceDir, ".claude") {
		t.Errorf("~/.claude link = %q err=%v", target, err)
	}
}

func TestRunResumePushesNoPR(t *testing.T) {
	cfg := mustCfg(t, strings.Replace(validMMDS,
		`"task": {"mode":"initial","prompt":"add a healthcheck"}`,
		`"task": {"mode":"resume","instructions":["address review comments"]}`, 1))
	sup, ft, fr, fa := newTestSupervisor(t, cfg)

	// Make Prepare take the fetch path: an existing clone on the workspace.
	if err := os.MkdirAll(filepath.Join(sup.repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A prior Claude session to resume.
	if err := os.MkdirAll(sup.fleetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sup.fleetDir, "claude-session"), []byte("claude-uuid"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr.outputs["status --porcelain"] = " M main.go\n"
	fr.outputs["rev-parse HEAD"] = "def5678\n"

	if err := sup.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := singleEvent(t, ft)
	if ev.Type != EventPushDone || ev.CommitSHA != "def5678" {
		t.Fatalf("event = %+v, want push_done def5678", ev)
	}
	if fr.ran("gh pr create") {
		t.Error("resume must not open a new PR")
	}
	if !fa.gotSpec.Resume || fa.gotSpec.ClaudeSessionID != "claude-uuid" {
		t.Errorf("resume spec = %+v, want Resume + ClaudeSessionID", fa.gotSpec)
	}
	if !fr.ran("fetch origin") {
		t.Error("resume should fetch, not clone")
	}
}

func TestRunAgentFailurePublishesAgentFailed(t *testing.T) {
	sup, ft, fr, fa := newTestSupervisor(t, mustCfg(t, validMMDS))
	fa.err = errors.New("model exploded")

	if err := sup.Run(context.Background()); err == nil {
		t.Fatal("Run should return the agent error")
	}
	ev := singleEvent(t, ft)
	if ev.Type != EventAgentFailed || ev.Stage != "agent" {
		t.Fatalf("event = %+v, want agent_failed stage=agent", ev)
	}
	if fr.ran("gh pr create") {
		t.Error("no PR should be opened on agent failure")
	}
}

func TestCommitMessageUsesGeneratedThenFallsBack(t *testing.T) {
	sup, _, _, fa := newTestSupervisor(t, mustCfg(t, validMMDS))

	// When the generator returns a message, it is used verbatim.
	sup.commitMsg = func(_ context.Context, req, summary, _, _ string) string {
		if req == "" || summary != "did the thing" {
			t.Errorf("generator got req=%q summary=%q", req, summary)
		}
		return "feat: add a healthcheck endpoint"
	}
	if got := sup.commitMessageFor(context.Background(), fa.result, "stat", "patch"); got != "feat: add a healthcheck endpoint" {
		t.Errorf("generated message = %q", got)
	}

	// When it returns "", we fall back to the static template (never an empty commit message).
	sup.commitMsg = func(context.Context, string, string, string, string) string { return "" }
	if got := sup.commitMessageFor(context.Background(), fa.result, "stat", "patch"); got == "" ||
		!strings.HasPrefix(got, "agent: ") {
		t.Errorf("fallback message = %q, want the static 'agent: …' template", got)
	}
}

func TestRunNoChangesClosesCleanly(t *testing.T) {
	sup, ft, fr, _ := newTestSupervisor(t, mustCfg(t, validMMDS))
	fr.outputs["status --porcelain"] = "" // agent produced no diff

	if err := sup.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	ev := singleEvent(t, ft)
	if ev.Type != EventPushDone || ev.Stage != "no_changes" {
		t.Fatalf("event = %+v, want push_done no_changes", ev)
	}
	if fr.ran("push") || fr.ran("gh pr create") {
		t.Error("no push/PR when there are no changes")
	}
}
