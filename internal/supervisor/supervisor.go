package supervisor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
// into a PR (initial) or a push (resume).
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
	prOpened     bool          // this guest has already opened the PR — later turns push, don't re-open
	// commitMsg writes a commit message from the diff (request, agent summary, --stat, patch),
	// returning "" to fall back to the static template. A field so tests inject a stub instead of
	// making a real gateway call.
	commitMsg func(ctx context.Context, request, agentSummary, diffStat, diffPatch string) string

	// Human-attach tunnel (`code tunnel`) state — see tunnel.go. runCtx is the Run lifetime, so a
	// tunnel started from a command handler dies with the session.
	runCtx       context.Context
	tunnelMu     sync.Mutex
	tunnelOn     bool
	tunnelCancel context.CancelFunc
	tunnelWG     sync.WaitGroup // tracks the tunnel goroutine so teardown waits for it to unregister
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
	s := &Supervisor{
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
	s.commitMsg = func(ctx context.Context, request, agentSummary, diffStat, diffPatch string) string {
		// The commit-message helper follows the active provider, same as the agent: subscription talks
		// to Anthropic directly on the OAuth token; otherwise the host gateway on the session token.
		baseURL, token := cfg.LLM.BaseURL, cfg.LLM.AuthToken
		if cfg.Agent.Auth == "subscription" && cfg.Agent.OAuthToken != "" {
			baseURL, token = anthropicDirectURL, cfg.Agent.OAuthToken
		}
		return GenerateCommitMessage(ctx, baseURL, token, cfg.Agent.CheapModel,
			request, agentSummary, diffStat, diffPatch)
	}
	return s
}

// Run executes the lifecycle: one turn per round, staying warm between rounds so a follow-up
// message is handled without a cold relaunch (keep-alive). It publishes one event
// per turn (pr_opened / push_done / agent_failed) and a session_idle event when it parks.
func (s *Supervisor) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.runCtx = ctx
	defer s.shutdownTunnel() // stop + unregister any attach tunnel so it doesn't leak an account slot

	wake := make(chan struct{}, 1)
	s.startHeartbeat(ctx)
	s.subscribeCommands(cancel, wake)

	if err := s.deps.Platform.MountWorkspace(workspaceDevice, s.workspaceDir); err != nil {
		return s.failf(err, "mount_workspace")
	}
	if err := s.linkClaudeHome(); err != nil {
		return s.failf(err, "link_claude")
	}
	// Drop the pre-authenticated `code tunnel` token into place so a human attach skips the device
	// login (best-effort — no-op if none was provisioned).
	if err := s.writeVSCodeAuth(""); err != nil {
		s.deps.Log.Warn("vscode auth", "err", err)
	}
	if err := s.git.SetupCredentials(ctx); err != nil {
		return s.failf(err, "git_credentials")
	}
	if err := s.git.Prepare(ctx); err != nil {
		return s.failf(err, "git_prepare")
	}

	// First turn: initial (opens the PR) or a cold resume (pushes), per the launch mode.
	if err := s.turn(ctx, s.firstTurnSpec(), s.shouldOpenPR()); err != nil {
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
			if s.tunnelActive() {
				idle.Reset(s.keepAlive) // a human is attached via the tunnel — don't park under them
				continue
			}
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
				if err := s.turn(ctx, s.resumeSpec(nil, queued), s.shouldOpenPR()); err != nil {
					return err
				}
			}
			idle.Reset(s.keepAlive)
		}
	}
}

// shouldOpenPR reports whether a turn that produces changes should open the PR: only for an
// initial-mode session that hasn't opened one yet. This lets a later turn open the PR when the
// first turn made no changes (e.g. the operator dispatched a placeholder, then supplied the plan) —
// while a resume-mode session (its PR already exists) never opens a second one.
func (s *Supervisor) shouldOpenPR() bool {
	return s.cfg.Task.Mode == ModeInitial && !s.prOpened
}

// turn runs one agent turn and finalizes it (commit/push; open the PR when shouldOpenPR and the
// turn changed something). A failure publishes agent_failed and ends the session.
func (s *Supervisor) turn(ctx context.Context, spec AgentSpec, openPR bool) error {
	turnStart := time.Now() // to scope the screenshot harvest to captures made during THIS turn
	res, err := s.deps.Agent.Run(ctx, spec, func(line []byte) {
		if err := s.deps.Bus.Log(line); err != nil {
			s.deps.Log.Warn("publish log line failed", "err", err)
		}
	})
	if err != nil {
		return s.failf(err, "agent")
	}
	s.persistClaudeSession(res.SessionID)
	return s.finalizeTurn(ctx, res, openPR, turnStart)
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
		Auth: s.cfg.Agent.Auth, OAuthToken: s.cfg.Agent.OAuthToken,
		Prompt: s.autonomousPreambleText() + s.cfg.Task.Prompt, SystemPrompt: s.cfg.Agent.Instructions,
	}
}

// autonomousPreambleText / collaboratePreambleText return the operator's box-side override when set,
// else the built-in default. This is how a box customizes the agent's base system prompt.
func (s *Supervisor) autonomousPreambleText() string {
	if p := strings.TrimSpace(s.cfg.Agent.PreambleAutonomous); p != "" {
		return s.cfg.Agent.PreambleAutonomous + "\n\n"
	}
	return autonomousPreamble
}

func (s *Supervisor) collaboratePreambleText() string {
	if p := strings.TrimSpace(s.cfg.Agent.PreambleCollaborate); p != "" {
		return s.cfg.Agent.PreambleCollaborate + "\n\n"
	}
	return collaboratePreamble
}

// resumeSpec builds a --resume spec that continues the same Claude Code session.
func (s *Supervisor) resumeSpec(instructions, queued []string) AgentSpec {
	return AgentSpec{
		WorkDir: s.repoDir, Model: s.cfg.Claude.Model,
		BaseURL: s.cfg.LLM.BaseURL, AuthToken: s.cfg.LLM.AuthToken,
		Prompt: resumePrompt(s.collaboratePreambleText(), instructions, queued), Resume: true,
		Auth: s.cfg.Agent.Auth, OAuthToken: s.cfg.Agent.OAuthToken,
		ClaudeSessionID: s.readClaudeSession(), SystemPrompt: s.cfg.Agent.Instructions,
	}
}

// finalizeTurn commits, pushes, and reports the turn. openPR opens the PR (first initial turn);
// otherwise it is a push to the existing branch.
func (s *Supervisor) finalizeTurn(ctx context.Context, res Result, openPR bool, turnStart time.Time) error {
	tokens := res.Usage.InputTokens + res.Usage.OutputTokens
	// The screenshot the agent chose to share this turn (if any), attached to whichever outcome we
	// publish below — including a no-op turn, since "show me a screenshot" is a legitimate no-change turn.
	shot := s.harvestScreenshot(turnStart)

	// Look at what changed before committing: a clean tree is the no-op turn, and the diff is what
	// lets a cheap model write a real commit message instead of a fixed placeholder.
	summary, patch, changed, err := s.git.PendingDiff(ctx)
	if err != nil {
		return s.failf(err, "git_diff")
	}
	if !changed {
		// A no-op turn is a legitimate outcome, not a crash — it's usually the agent
		// answering a question rather than editing. Carry its words so the thread stays a
		// conversation, not a bare "no changes".
		s.deps.Log.Info("agent produced no changes")
		return s.deps.Bus.Event(Event{Type: EventPushDone, Stage: "no_changes",
			Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens, Screenshot: shot})
	}

	sha, _, err := s.git.Commit(ctx, s.commitMessageFor(ctx, res, summary, patch))
	if err != nil {
		return s.failf(err, "git_commit")
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
		s.prOpened = true // later turns push to this PR instead of opening another
		return s.deps.Bus.Event(Event{Type: EventPROpened, PRNumber: number, PRURL: url,
			CommitSHA: sha, Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens, Screenshot: shot})
	}
	return s.deps.Bus.Event(Event{Type: EventPushDone, CommitSHA: sha,
		Reply: res.Result, CostUSD: res.TotalCostUSD, Tokens: tokens, Screenshot: shot})
}

// shareScreenshotName is the ONE file the agent writes to explicitly opt a screenshot into being shared
// with the reviewer (visualCheck preamble). Self-verification captures under other names in .mando/ are
// the agent's own and are never posted — sharing is on-demand (the reviewer asked, or the agent judged
// the result worth showing), not automatic on every visual change.
const shareScreenshotName = "share.png"

// harvestScreenshot returns the screenshot the agent chose to share THIS turn — the file
// .mando/share.png, if it was (re)written during this turn. The agent keeps .mando/ git-ignored
// (visualCheck preamble), so it's present on disk but uncommitted at finalize. Best-effort: returns nil
// on any problem (not shared, stale, oversize, symlink, read error) — sharing must never affect the
// turn's outcome.
//
// turnStart scopes it to this turn, so a share.png left over from an earlier turn is not re-posted; the
// agent overwrites it, so a shared screenshot always reflects the current state.
//
// The cap is sized against the TIGHTEST downstream hop: the PNG rides Event.Screenshot into the
// RunAgentPhase activity's result, which Temporal persists in workflow history under a default 2 MiB
// blob-size limit (base64 inflates ~4/3). A 1 MiB PNG (~1.4 MiB base64) leaves headroom for the rest of
// the result; anything larger is dropped rather than risk failing the turn's real outcome. (Far under
// the 8 MiB NATS max_payload the event also crosses.)
func (s *Supervisor) harvestScreenshot(turnStart time.Time) []byte {
	const maxScreenshotBytes = 1 << 20 // 1 MiB PNG; base64 ~1.4 MiB, under Temporal's 2 MiB blob limit
	// Open with O_NOFOLLOW and validate + read the SAME descriptor, so the path can't be a symlink
	// pointed at a token file: O_NOFOLLOW refuses a symlink, and fstat/read operate on that fd only.
	f, err := os.OpenFile(filepath.Join(s.repoDir, ".mando", shareScreenshotName), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || !info.ModTime().After(turnStart) {
		return nil
	}
	if info.Size() > maxScreenshotBytes {
		s.deps.Log.Info("shared screenshot too large to send", "bytes", info.Size())
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, maxScreenshotBytes)) // bound the read to the bytes, not just the stat
	if err != nil {
		return nil
	}
	return b
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
// turn boundary; abort cancels the run. `claude -p` is not interactive, so a message
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
		case CommandAttach:
			s.deps.Log.Info("attach requested — starting tunnel")
			// The tunnel token rides the attach command (delivered only when an operator attaches),
			// so it never sits in a non-attached guest.
			if err := s.writeVSCodeAuth(c.Text); err != nil {
				s.deps.Log.Warn("write vscode auth failed", "err", err)
			}
			s.startTunnel()
		case CommandDetach:
			s.deps.Log.Info("detach requested — stopping tunnel")
			s.detach()
		}
	})
	if err != nil {
		s.deps.Log.Warn("command subscription failed", "err", err)
	}
}

// linkClaudeHome symlinks ~/.claude to the workspace so Claude Code's transcript persists,
// which is what makes --resume work in a fresh VM days later (the load-bearing line).
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

// vscodeDataDir is the `code tunnel` CLI data dir. We pin it (rather than let the CLI default to
// $HOME/.vscode/cli) so writeVSCodeAuth and runTunnel always agree on where the auth token lives,
// independent of HOME — and put it on the workspace volume, which is reliably writable and persists
// across a resume. runTunnel passes this same path via --cli-data-dir.
func (s *Supervisor) vscodeDataDir() string { return filepath.Join(s.fleetDir, "vscode-cli") }

// writeVSCodeAuth drops the pre-authenticated `code tunnel` token (from the boot config) into the
// CLI's data dir, so a human attach skips the GitHub device login. No-op when none was provisioned
// (then the operator device-logs-in on first attach).
func (s *Supervisor) writeVSCodeAuth(token string) error {
	tok := strings.TrimSpace(token)
	if tok == "" {
		tok = strings.TrimSpace(s.cfg.VSCode.TunnelToken) // legacy fallback (token injected via MMDS)
	}
	if tok == "" {
		return nil
	}
	dir := s.vscodeDataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "token.json"), []byte(tok), 0o600)
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

// commitMessageFor writes the commit message from the actual diff via a cheap model, so history
// reflects what changed and why rather than a fixed line. Falls back to the static template if the
// model is unavailable or returns nothing — a commit must never hinge on the LLM.
func (s *Supervisor) commitMessageFor(ctx context.Context, res Result, summary, patch string) string {
	if s.commitMsg != nil {
		if msg := s.commitMsg(ctx, s.commitAsk(), res.Result, summary, patch); msg != "" {
			return msg
		}
	}
	return s.commitMessage()
}

// commitAsk is the request context handed to the commit-message model: the original prompt on the
// first run, the reviewer's instructions on a resume.
func (s *Supervisor) commitAsk() string {
	if s.cfg.Task.Mode == ModeResume {
		if ask := strings.TrimSpace(strings.Join(s.cfg.Task.Instructions, "\n")); ask != "" {
			return ask
		}
		return "Address the reviewer's feedback on the open PR."
	}
	return s.cfg.Task.Prompt
}

// commitMessage is the static fallback used only when the cheap-model call fails.
func (s *Supervisor) commitMessage() string {
	if s.cfg.Task.Mode == ModeResume {
		return fmt.Sprintf("agent: address review feedback (%s)", s.cfg.SessionID)
	}
	return fmt.Sprintf("agent: %s", firstLine(s.cfg.Task.Prompt))
}

func (s *Supervisor) prTitle() string { return firstLine(s.cfg.Task.Prompt) }

// prBody frames the PR: the task as a blockquote, then the agent's own summary — which now carries
// its Verification and Risks sections (see selfReview) so a reviewer gets evidence and the agent's
// honest uncertainty inline, not just a diff.
func (s *Supervisor) prBody(res Result) string {
	var b strings.Builder
	prompt := strings.TrimSpace(s.cfg.Task.Prompt)
	fmt.Fprintf(&b, "**Task**\n> %s\n\n---\n\n", strings.ReplaceAll(prompt, "\n", "\n> "))
	if r := strings.TrimSpace(res.Result); r != "" {
		b.WriteString(r)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\n_Opened by the agent fleet · session `%s` · cost $%.4f_\n", s.cfg.SessionID, res.TotalCostUSD)
	return b.String()
}

// autonomousPreamble prefixes every task: the agent runs headless with no human to answer
// questions, so it must make reasonable assumptions and finish rather than ask. It must
// NOT run git or gh itself — the supervisor commits, pushes, and opens/updates the PR after the
// agent finishes (finalize). If the agent commits, `git status` is clean and the supervisor
// sees no diff, so the work is never pushed.
const autonomousPreamble = "You are running non-interactively, with no human available to " +
	"answer questions. Make reasonable assumptions and COMPLETE the task by editing files — " +
	"do not ask for clarification or stop to confirm. Apply real engineering judgment: if the " +
	"task is flawed, risky, or there is a clearly better approach, do the better thing and " +
	"explain why in your summary rather than following a bad instruction literally. Do NOT run " +
	"git commit, git push, or gh, and do NOT open a pull request: committing, pushing, and the " +
	"PR are handled automatically once you finish. Just make the file changes. " +
	artifactHygiene + " " + selfReview + " " + visualCheck + "\n\n"

// selfReview makes every code change arrive with its own evidence, so a reviewer can trust it
// cheaply instead of re-deriving what it does and whether it works. The agent verifies its own
// work and states, honestly, where it's unsure — the highest-leverage thing for making review
// scale (the doer knows its risks better than a reviewer reading the diff).
const selfReview = "Whenever you change code, verify it before finishing: if the project has " +
	"tests, run the ones relevant to your change (and add a test when you add new behavior), and " +
	"run whatever linter or build the repo uses. You CAN install what you need: this sandbox " +
	"reaches the git, npm, PyPI, and Go module registries through a proxy that is already set in " +
	"your environment, so install the project's real dependencies and run its ACTUAL test suite " +
	"rather than hand-checking or assuming (pip and python3-venv are available; a direct package " +
	"download that fails usually just means that registry isn't on the egress allowlist — say so " +
	"rather than giving up on verification). To learn how this project installs its " +
	"dependencies and runs its tests, treat its own CI config (for example the files under " +
	".github/workflows) or its README/CONTRIBUTING as the source of truth — those are the exact " +
	"commands the maintainers use — rather than guessing at them. Then end your final message with " +
	"two short sections — a '## Verification' section giving the exact commands you ran and their " +
	"outcome (or, honestly, why you couldn't run them), and a '## Risks' section naming the " +
	"riskiest part of the change and anything you're genuinely unsure about, so the reviewer knows " +
	"where to look (write 'None — straightforward' only if that is truly the case). Never claim a " +
	"test passed that you did not actually run."

// artifactHygiene is shared by both preambles: the supervisor commits everything present in the
// tree (git add -A), so build artifacts and caches left behind end up in the PR. The agent can't
// run git, but it can edit a .gitignore — so it must keep the tree to source + intended files.
const artifactHygiene = "Commit only source and intended files. If your work produces build " +
	"artifacts or caches (for example __pycache__/, *.pyc, node_modules/, dist/, build/, " +
	".pytest_cache/, coverage files), do NOT leave them in the working tree — everything present " +
	"gets committed. Add or update a .gitignore to exclude them (create one if the repo has none), " +
	"and delete any that already slipped in."

// visualCheck gives the agent eyes: for a change with a visible browser effect it renders the result
// with a real headless browser (mando-shot, baked into the image) and reviews the screenshot, so
// "looks right in code, broken in render" gets caught before the PR. Conditional and fail-soft — a
// non-bootable app never blocks the change. See docs/preview.md.
const visualCheck = "If your change has a visible effect in a browser (UI, styling, layout, a page or " +
	"component), verify it visually before finishing. Bring up a preview — use the preview: block in " +
	".mandobox.yml if the repo has one (its start command, port, and path); otherwise infer the " +
	"dev-server or Storybook command and port from package.json — then capture the relevant page with " +
	"`mando-shot <url>` (a preinstalled command; run `mando-shot --help`) and READ the resulting PNG to " +
	"check the change you were asked for is actually visible and nothing is obviously broken (overlap, " +
	"cut-off text, broken layout). Fix and re-capture, at most about three times. Prefer rendering a " +
	"single component or Storybook story over booting the whole app when you can — it is faster and more " +
	"reliable. If you genuinely cannot get a meaningful render (it needs a backend, secrets, seed data, " +
	"or a login), say so plainly in your summary and move on — never block the change on it. These " +
	"verification captures are for YOU and are NOT shown to the reviewer by default — a screenshot on " +
	"every change is noise. But when the reviewer ASKS to see a screenshot, or a visual result is " +
	"genuinely worth showing them, you SHARE one by capturing it to the exact path `.mando/share.png` — " +
	"run `mando-shot <url> --out .mando/share.png`. That one file is delivered into the reviewer's chat " +
	"as an actual image; a written description is NOT a substitute, so when they ask to see something, " +
	"capture it there rather than only describing it. It reflects THIS turn, so re-capture it after each " +
	"change they want to see. Keep the capture directory out of the commit (put .mando/ in .gitignore)."

// DefaultAutonomousPreamble / DefaultCollaboratePreamble expose the built-in preambles so the worker
// can materialize them to disk for the dashboard (which shows them as the editable default / reset
// baseline). They are the canonical source; an operator override replaces them per box.
const (
	DefaultAutonomousPreamble  = autonomousPreamble
	DefaultCollaboratePreamble = collaboratePreamble
)

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
	"you. " + artifactHygiene + " " + selfReview + " " + visualCheck + " Your final message is posted straight back to " +
	"the reviewer as your reply, so address them directly.\n\n"

// resumePrompt assembles a resume turn from the reviewer's messages.
func resumePrompt(preamble string, instructions, queued []string) string {
	var b strings.Builder
	b.WriteString(preamble)
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
