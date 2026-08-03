package supervisor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	workspaceDevice   = "/dev/vdb"
	heartbeatInterval = 30 * time.Second
	// keepAliveWindow is the guest-side idle backstop: how long the VM stays warm waiting for a
	// follow-up message before parking itself. Kept well above the control plane's keep-alive
	// timer so the control plane always tears down first; this only fires if it is gone.
	keepAliveWindow = 20 * time.Minute
)

// Deps are the injected collaborators, so orchestration is unit-testable with fakes.
type Deps struct {
	Bus      *Bus
	Runner   Runner
	Agent    AgentRunner
	Platform Platform
	Log      *slog.Logger
}

// Supervisor runs the guest-side lifecycle after bootstrap (network + MMDS + transport are
// established by the caller). It mounts the workspace, runs the agent, and turns the result
// into a PR (initial) or a push (resume) — PLAN §8.1.
type Supervisor struct {
	cfg          BootConfig
	deps         Deps
	workspaceDir string
	repoDir      string
	fleetDir     string
	git          *Git
	queue        *Queue
	home         string
	keepAlive    time.Duration // idle backstop before the warm VM parks itself
}

// New builds a Supervisor rooted at workspaceDir (the mount point of the persistent volume).
func New(cfg BootConfig, deps Deps, workspaceDir string) *Supervisor {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	fleetDir := filepath.Join(workspaceDir, ".fleet")
	repoDir := filepath.Join(workspaceDir, "repo")
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return &Supervisor{
		cfg:          cfg,
		deps:         deps,
		workspaceDir: workspaceDir,
		repoDir:      repoDir,
		fleetDir:     fleetDir,
		git:          NewGit(deps.Runner, cfg, repoDir, fleetDir),
		queue:        NewQueue(filepath.Join(fleetDir, "steering-queue.jsonl")),
		home:         home,
		keepAlive:    keepAliveWindow,
	}
}

// Run executes the lifecycle: one turn per round, staying warm between rounds so a follow-up
// message is handled without a cold relaunch (§6.1 keep-alive, §8.3). It publishes one event
// per turn (pr_opened / push_done / agent_failed) and a session_idle event when it parks.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wake := make(chan struct{}, 1)
	s.startHeartbeat(ctx)
	s.subscribeCommands(cancel, wake)

	if err := s.deps.Platform.MountWorkspace(workspaceDevice, s.workspaceDir); err != nil {
		return s.failf(err, "mount_workspace")
	}
	if err := s.linkClaudeHome(); err != nil {
		return s.failf(err, "link_claude")
	}
	if err := s.git.SetupCredentials(ctx); err != nil {
		return s.failf(err, "git_credentials")
	}
	if err := s.git.Prepare(ctx); err != nil {
		return s.failf(err, "git_prepare")
	}

	// First turn: initial (opens the PR) or a cold resume (pushes), per the launch mode.
	if err := s.turn(ctx, s.firstTurnSpec(), s.cfg.Task.Mode == ModeInitial); err != nil {
		return err // failf already published agent_failed
	}

	// Keep-alive loop: stay warm and handle delivered messages turn-by-turn until the session
	// idles out or is aborted. The control plane tears the VM down sooner (on merge, its own
	// keep-alive timer, or abort); this idle backstop only fires if the control plane is gone.
	idle := time.NewTimer(s.keepAlive)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done(): // abort
			return nil
		case <-idle.C:
			_ = s.deps.Bus.Event(Event{Type: EventSessionIdle})
			s.deps.Log.Info("session idle — parking")
			return nil
		case <-wake:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			queued, err := s.queue.Drain()
			if err != nil {
				return s.failf(err, "queue_drain")
			}
			if len(queued) > 0 {
				if err := s.turn(ctx, s.resumeSpec(nil, queued), false); err != nil {
					return err
				}
			}
			idle.Reset(s.keepAlive)
		}
	}
}

// turn runs one agent turn and finalizes it (commit/push; open a PR only on the first initial
// turn). A failure publishes agent_failed and ends the session.
func (s *Supervisor) turn(ctx context.Context, spec AgentSpec, openPR bool) error {
	res, err := s.deps.Agent.Run(ctx, spec, func(line []byte) {
		if err := s.deps.Bus.Log(line); err != nil {
			s.deps.Log.Warn("publish log line failed", "err", err)
		}
	})
	if err != nil {
		return s.failf(err, "agent")
	}
	s.persistClaudeSession(res.SessionID)
	return s.finalizeTurn(ctx, res, openPR)
}

// firstTurnSpec builds the first turn's spec from the launch mode.
func (s *Supervisor) firstTurnSpec() AgentSpec {
	if s.cfg.Task.Mode == ModeResume {
		queued, _ := s.queue.Drain()
		return s.resumeSpec(s.cfg.Task.Instructions, queued)
	}
	return AgentSpec{
		WorkDir: s.repoDir, Model: s.cfg.Claude.Model,
		BaseURL: s.cfg.LLM.BaseURL, AuthToken: s.cfg.LLM.AuthToken,
		Prompt: autonomousPreamble + s.cfg.Task.Prompt,
	}
}

// resumeSpec builds a --resume spec that continues the same Claude Code session.
func (s *Supervisor) resumeSpec(instructions, queued []string) AgentSpec {
	return AgentSpec{
		WorkDir: s.repoDir, Model: s.cfg.Claude.Model,
		BaseURL: s.cfg.LLM.BaseURL, AuthToken: s.cfg.LLM.AuthToken,
		Prompt: resumePrompt(instructions, queued), Resume: true,
		ClaudeSessionID: s.readClaudeSession(),
	}
}

// finalizeTurn commits, pushes, and reports the turn. openPR opens the PR (first initial turn);
// otherwise it is a push to the existing branch.
func (s *Supervisor) finalizeTurn(ctx context.Context, res Result, openPR bool) error {
	sha, changed, err := s.git.Commit(ctx, s.commitMessage())
	if err != nil {
		return s.failf(err, "git_commit")
	}
	tokens := res.Usage.InputTokens + res.Usage.OutputTokens
	if !changed {
		// A no-op turn is a legitimate outcome, not a crash (§13) — it's usually the agent
		// answering a question rather than editing. Carry its words so the thread stays a
		// conversation, not a bare "no changes".
		s.deps.Log.Info("agent produced no changes")
		return s.deps.Bus.Event(Event{Type: EventPushDone, Stage: "no_changes",
			Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens})
	}
	if err := s.git.Push(ctx); err != nil {
		return s.failf(err, "git_push")
	}
	if openPR {
		number, url, err := s.git.OpenPR(ctx, s.prTitle(), s.prBody(res))
		if err != nil {
			return s.failf(err, "open_pr")
		}
		s.deps.Log.Info("opened PR", "number", number, "url", url)
		return s.deps.Bus.Event(Event{Type: EventPROpened, PRNumber: number, PRURL: url,
			CommitSHA: sha, Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens})
	}
	return s.deps.Bus.Event(Event{Type: EventPushDone, CommitSHA: sha,
		Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens})
}

// failf publishes an agent_failed event and returns the wrapped error.
func (s *Supervisor) failf(err error, stage string) error {
	s.deps.Log.Error("stage failed", "stage", stage, "err", err)
	_ = s.deps.Bus.Event(Event{Type: EventAgentFailed, Stage: stage, Error: err.Error()})
	return fmt.Errorf("%s: %w", stage, err)
}

func (s *Supervisor) startHeartbeat(ctx context.Context) {
	go func() {
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.deps.Bus.Heartbeat(); err != nil {
					s.deps.Log.Warn("heartbeat failed", "err", err)
				}
			}
		}
	}()
}

// subscribeCommands queues user_message and wakes the keep-alive loop to handle it at the next
// turn boundary; abort cancels the run (§8.3). `claude -p` is not interactive, so a message
// cannot be injected mid-turn — messages that arrive during a turn are handled together next.
func (s *Supervisor) subscribeCommands(cancel context.CancelFunc, wake chan<- struct{}) {
	err := s.deps.Bus.OnCommand(func(c Command) {
		switch c.Type {
		case CommandUserMessage:
			if err := s.queue.Append(c.Text); err != nil {
				s.deps.Log.Warn("queue user_message failed", "err", err)
			}
			select {
			case wake <- struct{}{}: // non-blocking: coalesces a burst into one wake
			default:
			}
		case CommandAbort:
			s.deps.Log.Warn("abort received", "reason", c.Reason)
			cancel()
		}
	})
	if err != nil {
		s.deps.Log.Warn("command subscription failed", "err", err)
	}
}

// linkClaudeHome symlinks ~/.claude to the workspace so Claude Code's transcript persists,
// which is what makes --resume work in a fresh VM days later (§8.1 — the load-bearing line).
func (s *Supervisor) linkClaudeHome() error {
	target := filepath.Join(s.workspaceDir, ".claude")
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	link := filepath.Join(s.home, ".claude")
	if err := os.MkdirAll(s.home, 0o700); err != nil {
		return err
	}
	_ = os.RemoveAll(link)
	return os.Symlink(target, link)
}

func (s *Supervisor) claudeSessionFile() string {
	return filepath.Join(s.fleetDir, "claude-session")
}

func (s *Supervisor) persistClaudeSession(id string) {
	if id == "" {
		return
	}
	if err := os.MkdirAll(s.fleetDir, 0o700); err != nil {
		s.deps.Log.Warn("persist claude session: mkdir", "err", err)
		return
	}
	if err := os.WriteFile(s.claudeSessionFile(), []byte(id), 0o600); err != nil {
		s.deps.Log.Warn("persist claude session", "err", err)
	}
}

func (s *Supervisor) readClaudeSession() string {
	b, err := os.ReadFile(s.claudeSessionFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Supervisor) commitMessage() string {
	if s.cfg.Task.Mode == ModeResume {
		return fmt.Sprintf("agent: address review feedback (%s)", s.cfg.SessionID)
	}
	return fmt.Sprintf("agent: %s", firstLine(s.cfg.Task.Prompt))
}

func (s *Supervisor) prTitle() string { return firstLine(s.cfg.Task.Prompt) }

func (s *Supervisor) prBody(res Result) string {
	var b strings.Builder
	b.WriteString(s.cfg.Task.Prompt)
	b.WriteString("\n\n---\n")
	if res.Result != "" {
		b.WriteString(res.Result)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Opened by the agent fleet · session `%s` · cost $%.4f\n", s.cfg.SessionID, res.TotalCostUSD)
	return b.String()
}

// autonomousPreamble prefixes every task: the agent runs headless with no human to answer
// questions, so it must make reasonable assumptions and finish rather than ask (§8.3). It must
// NOT run git or gh itself — the supervisor commits, pushes, and opens/updates the PR after the
// agent finishes (finalize). If the agent commits, `git status` is clean and the supervisor
// sees no diff, so the work is never pushed.
const autonomousPreamble = "You are running non-interactively, with no human available to " +
	"answer questions. Make reasonable assumptions and COMPLETE the task by editing files — " +
	"do not ask for clarification or stop to confirm. Apply real engineering judgment: if the " +
	"task is flawed, risky, or there is a clearly better approach, do the better thing and " +
	"explain why in your summary rather than following a bad instruction literally. Do NOT run " +
	"git commit, git push, or gh, and do NOT open a pull request: committing, pushing, and the " +
	"PR are handled automatically once you finish. Just make the file changes (and run tests or " +
	"tools if useful).\n\n"

// collaboratePreamble frames a resume turn as a chat with the reviewer: answer questions, make
// changes when asked, keep the final message conversational (it is posted back as the reply).
// Not autonomousPreamble — that pushes "edit files, don't ask", which is wrong for a question.
const collaboratePreamble = "You are collaborating on an open pull request with a human reviewer, " +
	"as a thoughtful senior engineering peer — NOT an order-taker. Think hard about each message " +
	"and evaluate it on its merits before doing anything. If you agree it is the right move, do it. " +
	"But if you see a problem, a risk, a simpler or better approach, or you disagree, SAY SO: " +
	"explain your reasoning and argue for (or make) the better change instead of blindly complying. " +
	"A well-reasoned pushback or a better idea is worth far more to them than obedience. Never just " +
	"agree to be agreeable. If a message is a question or discussion, answer it thoughtfully and " +
	"substantively — do not change files. If it requests a change you judge to be right, make it by " +
	"editing files; if you think it is wrong or there is a better path, do that and explain, or ask " +
	"one sharp clarifying question. Your previous work is already in this workspace on its branch. " +
	"Do NOT run git commit, git push, or gh, and do NOT open a pull request — that is handled for " +
	"you. Your final message is posted straight back to the reviewer as your reply, so address them " +
	"directly.\n\n"

// resumePrompt assembles a resume turn from the reviewer's messages (§8.2).
func resumePrompt(instructions, queued []string) string {
	var b strings.Builder
	b.WriteString(collaboratePreamble)
	for _, in := range instructions {
		fmt.Fprintf(&b, "- %s\n", in)
	}
	for _, q := range queued {
		fmt.Fprintf(&b, "- %s\n", q)
	}
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 72 {
		s = s[:72]
	}
	if s == "" {
		return "agent changes"
	}
	return s
}
