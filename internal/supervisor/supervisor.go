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
	}
}

// Run executes the lifecycle. It publishes exactly one terminal event (pr_opened, push_done,
// or agent_failed) so the workflow always learns the outcome.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.startHeartbeat(ctx)
	s.subscribeCommands(cancel)

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

	res, err := s.runAgent(ctx)
	if err != nil {
		return s.failf(err, "agent")
	}
	s.persistClaudeSession(res.SessionID)

	return s.finalize(ctx, res)
}

// runAgent builds the prompt for the mode and runs Claude Code, streaming to NATS.
func (s *Supervisor) runAgent(ctx context.Context) (Result, error) {
	spec := AgentSpec{
		WorkDir:   s.repoDir,
		Model:     s.cfg.Claude.Model,
		BaseURL:   s.cfg.LLM.BaseURL,
		AuthToken: s.cfg.LLM.AuthToken,
	}
	if s.cfg.Task.Mode == ModeResume {
		queued, err := s.queue.Drain()
		if err != nil {
			return Result{}, err
		}
		spec.Prompt = resumePrompt(s.cfg.Task.Instructions, queued)
		spec.Resume = true
		spec.ClaudeSessionID = s.readClaudeSession()
	} else {
		spec.Prompt = s.cfg.Task.Prompt
	}
	return s.deps.Agent.Run(ctx, spec, func(line []byte) {
		if err := s.deps.Bus.Log(line); err != nil {
			s.deps.Log.Warn("publish log line failed", "err", err)
		}
	})
}

// finalize commits, pushes, and reports the outcome.
func (s *Supervisor) finalize(ctx context.Context, res Result) error {
	sha, changed, err := s.git.Commit(ctx, s.commitMessage())
	if err != nil {
		return s.failf(err, "git_commit")
	}
	if !changed {
		// A no-op run is a legitimate outcome, not a crash (§13).
		s.deps.Log.Info("agent produced no changes")
		return s.deps.Bus.Event(Event{Type: EventPushDone, Stage: "no_changes",
			CostUSD: res.TotalCostUSD, Tokens: res.Usage.InputTokens + res.Usage.OutputTokens})
	}
	if err := s.git.Push(ctx); err != nil {
		return s.failf(err, "git_push")
	}
	tokens := res.Usage.InputTokens + res.Usage.OutputTokens
	if s.cfg.Task.Mode == ModeInitial {
		number, url, err := s.git.OpenPR(ctx, s.prTitle(), s.prBody(res))
		if err != nil {
			return s.failf(err, "open_pr")
		}
		s.deps.Log.Info("opened PR", "number", number, "url", url)
		return s.deps.Bus.Event(Event{Type: EventPROpened, PRNumber: number, PRURL: url,
			CommitSHA: sha, CostUSD: res.TotalCostUSD, Tokens: tokens})
	}
	return s.deps.Bus.Event(Event{Type: EventPushDone, CommitSHA: sha,
		CostUSD: res.TotalCostUSD, Tokens: tokens})
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

// subscribeCommands queues user_message for the next round and cancels the run on abort
// (§8.3). `claude -p` is not interactive, so a message cannot be injected mid-turn.
func (s *Supervisor) subscribeCommands(cancel context.CancelFunc) {
	err := s.deps.Bus.OnCommand(func(c Command) {
		switch c.Type {
		case CommandUserMessage:
			if err := s.queue.Append(c.Text); err != nil {
				s.deps.Log.Warn("queue user_message failed", "err", err)
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

// resumePrompt assembles a resume instruction, folding in any queued steering messages, with
// an explicit directive to push to the same branch and NOT open a new PR (§8.2).
func resumePrompt(instructions, queued []string) string {
	var b strings.Builder
	b.WriteString("Continue work on this branch. Address the following feedback, then commit.\n")
	b.WriteString("Do NOT open a new pull request — push to the existing branch.\n\n")
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
