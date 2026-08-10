package control

import (
	"fmt"
	"strings"
	"time"

	"github.com/acme/mandobox/internal/supervisor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// maxSeenDeliveries bounds the dedupe set kept in workflow state. GitHub redelivers,
// so we drop already-seen delivery IDs; the set is trimmed FIFO to keep history small.
const maxSeenDeliveries = 500

// PRWorkflow owns one task from dispatch through the human review loop to teardown.
// It is deterministic: all I/O is in activities, all outside input arrives as signals.
func PRWorkflow(ctx workflow.Context, in WorkflowInput) (State, error) {
	in.Policy = in.Policy.withDefaults()
	log := workflow.GetLogger(ctx)

	st := &State{
		SessionID:  in.SessionID,
		Repo:       in.Repo,
		BaseBranch: in.BaseBranch,
		HeadBranch: "agent/" + in.SessionID,
		ImageSHA:   in.ImageSHA,
		VMState:    vmNone,
		Phase:      "starting",
	}
	if err := workflow.SetQueryHandler(ctx, "status", func() (State, error) { return *st, nil }); err != nil {
		return *st, err
	}
	// Findable by repo from the start; pr_number is added once the PR opens.
	_ = workflow.UpsertTypedSearchAttributes(ctx,
		temporal.NewSearchAttributeKeyKeyword(SARepo).ValueSet(in.Repo))

	// Give the branch a meaningful, task-derived name (git best practice) — agent/<slug>-<id-tail> —
	// instead of agent/<session_id>. Version-gated so a workflow recorded before this shipped keeps
	// its original branch and its PR reconcile still matches (GetVersion). The guest is told the
	// same branch via MMDS (repo.head_branch), so both sides push/reconcile the same ref.
	if workflow.GetVersion(ctx, "meaningful-branch", workflow.DefaultVersion, 1) >= 1 {
		var a *Activities
		var slug string
		_ = workflow.ExecuteActivity(mintCtx(ctx), a.SlugifyTask, in.Prompt).Get(ctx, &slug)
		if slug != "" {
			st.HeadBranch = "agent/" + slug + "-" + branchSuffix(in.SessionID)
		}
	}

	startTime := workflow.Now(ctx)
	postRoot(ctx, st, in) // opens the Slack thread; sets SlackThreadTS + SlackChannel

	// Resolve the effective config: the repo's .mandobox.yml folded with the box defaults/limits and
	// this task's overrides, clamped (docs/configuration.md). Version-gated so an in-flight workflow
	// keeps its already-applied input and doesn't diverge on replay (GetVersion).
	if workflow.GetVersion(ctx, "repo-config", workflow.DefaultVersion, 1) >= 1 {
		var a *Activities
		var rc ResolvedConfig
		if err := workflow.ExecuteActivity(mintCtx(ctx), a.ResolveConfig, in).Get(ctx, &rc); err == nil {
			in.VCPUs, in.MemMiB, in.Model = rc.VCPUs, rc.MemMiB, rc.Model
			in.Agent, in.Instructions = rc.Agent, rc.Instructions
			in.Policy.MaxReviewRounds = rc.Policy.MaxReviewRounds
			in.Policy.AutoFixCI = rc.Policy.AutoFixCI
			in.Policy.CostCeilingUSD = rc.Policy.CostCeilingUSD
			in.Policy.HardTTL = rc.Policy.HardTTL
			in.Policy.KeepAlive = rc.Policy.KeepAlive
			for _, w := range rc.Warnings {
				slackNote(ctx, st, ":information_source: "+w)
			}
		}
	}

	// Hard TTL bounds the whole workflow.
	ttl := workflow.NewTimer(ctx, in.Policy.HardTTL)

	// Initial phase: mint → launch → run, and keep the VM warm for the review that follows.
	res := launchWarm(ctx, in, st, supervisor.ModeInitial, nil)
	recordOutcome(ctx, st, in, res)

	// Read the no-PR-waits-for-input version gate before deciding how to report the initial turn (so
	// it sits above reportPhase and the command order is stable). A workflow recorded before this
	// shipped returns DefaultVersion and replays its original reporting + teardown (GetVersion).
	noPRWaitVersion := workflow.GetVersion(ctx, "no-pr-wait-for-input", workflow.DefaultVersion, 1)

	// A benign no-op first turn (it changed nothing, so no PR) usually means the operator deferred the
	// real input — a plan/spec they're about to drop, often a thread attachment landing a beat later.
	// In that case skip the agent's essay about the "missing" plan; the single clean waiting note
	// below reads far better. A PR opened, or a genuine failure, is reported as usual.
	benignNoOp := res.Outcome == supervisor.EventPushDone && st.PRNumber == 0
	if !(noPRWaitVersion >= 1 && benignNoOp) {
		reportPhase(ctx, st, res, false) // initial run — the PR-opened announcement belongs in Slack
	}

	// The pr_opened event travels over NATS (at-most-once); if it was lost — e.g. the run went
	// quiet and RunAgentPhase gave up while the guest went on to open the PR — reconcile with
	// GitHub so a real PR is never mistaken for "no PR".
	if st.PRNumber == 0 {
		reconcilePR(ctx, st)
	}

	// If the first run opened no PR: older workflows tear down and finish here. Newer ones keep the
	// session warm and wait for the operator to supply what was missing — a follow-up turn that opens
	// a real PR joins the review loop below; if nothing arrives before the warm window lapses, the
	// idle path ends it cleanly.
	noPRWait := false
	if st.PRNumber == 0 {
		if noPRWaitVersion == workflow.DefaultVersion {
			st.Phase = "no_pr"
			destroyWorkspace(ctx, st)
			finalSummary(ctx, st, startTime)
			return *st, nil
		}
		noPRWait = true
		slackNote(ctx, st, ":inbox_tray: Ready when you are — reply with the details, or drop your plan/spec in this thread, and I'll get to work.")
	}

	// ---- review loop ----
	seen := map[string]bool{}
	var seenOrder []string
	dedupe := func(id string) bool {
		if id == "" || seen[id] {
			return true // treat empty/duplicate as already-handled
		}
		seen[id] = true
		seenOrder = append(seenOrder, id)
		if len(seenOrder) > maxSeenDeliveries {
			delete(seen, seenOrder[0])
			seenOrder = seenOrder[1:]
		}
		return false
	}

	reviewComment := workflow.GetSignalChannel(ctx, SignalReviewComment)
	reviewSubmitted := workflow.GetSignalChannel(ctx, SignalReviewSubmitted)
	prClosed := workflow.GetSignalChannel(ctx, SignalPRClosed)
	ciStatus := workflow.GetSignalChannel(ctx, SignalCIStatus)
	userMessage := workflow.GetSignalChannel(ctx, SignalUserMessage)
	abort := workflow.GetSignalChannel(ctx, SignalAbort)
	attachCh := workflow.GetSignalChannel(ctx, SignalAttach)
	detachCh := workflow.GetSignalChannel(ctx, SignalDetach)

	var (
		closed, merged, aborted bool
		endedNoInput            bool // no PR ever opened and no follow-up arrived — end the idle session
		abortReason             string
		coalesce                workflow.Future // short window batching a burst into one turn
		keepAlive               workflow.Future // warmth timer; firing parks the idle VM
		pendingFromPR           bool            // batch includes GitHub feedback → mirror reply to the PR
		pendingReplyToID        int64           // inline review-comment id to thread the reply under
		ackN                    int             // rotates the whimsical ack word so it isn't "Looking…" every time
		attached                bool            // a human is in the VM via `code tunnel` — pin + pause
		awaitingChoice          bool            // detached with edits; waiting for commit|discard|handoff
		relay                   workflow.Future // RelayTunnel: streams tunnel output to Slack while attached
	)
	// frozen: a human is in the loop (attached, or deciding what to do with their edits) — pin the
	// VM and don't run the agent under them.
	frozen := func() bool { return attached || awaitingChoice }
	armKeepAlive := func() {
		// KeepAlive <= 0 means "never park": keep the VM warm for the PR's life (still bounded by
		// HardTTL). A running session you want to keep or attach to shouldn't be reaped on idle.
		// While a human is attached the VM is pinned (never park under them). Replay-safe for
		// existing workflows (their KeepAlive is a positive default; attached is always false there).
		if st.VMState == vmRunning && in.Policy.KeepAlive > 0 && !frozen() {
			keepAlive = workflow.NewTimer(ctx, in.Policy.KeepAlive)
		} else {
			keepAlive = nil
		}
	}

	// seenComments holds GitHub comment/review IDs already delivered, so the thread reconcile never
	// re-feeds them (and a webhook retry doesn't double-deliver). reconcileVersion gates the whole
	// reconcile feature: introducing it via GetVersion means an in-flight workflow (recorded before
	// this code shipped) replays its original behavior, while new workflows get the reconcile — so a
	// worker redeploy never stalls a live PR (the alternative — a short TTL — was rejected).
	seenComments := map[int64]bool{}
	reconcileVersion := workflow.GetVersion(ctx, "pr-thread-reconcile", workflow.DefaultVersion, 1)
	// editAckVersion gates the specific "taking care of your changes" ack for a detach-decision reply
	// (vs the generic whimsical ack). Version-gated so a live session that already handled an
	// edit-decision under the old code replays its original acks and a redeploy never stalls it.
	editAckVersion := workflow.GetVersion(ctx, "edit-decision-ack", workflow.DefaultVersion, 1)

	armKeepAlive()
	if noPRWait { // no PR yet — the "ready for your plan" note above already told them what to do
		st.Phase = "awaiting_input"
	} else {
		st.Phase = "awaiting_review"
		slackNote(ctx, st, ":speech_balloon: Session's warm — reply in this thread with any changes and I'll jump on them.")
	}
	// The session lives as long as the PR: it keeps answering comments (and keeps the workspace,
	// so the agent's transcript retains the whole PR conversation) until the PR is merged/closed
	// or aborted. Rounds and cost are tracked for the summary, not used to end the session — a
	// human-driven back-and-forth must not be cut off at an arbitrary count.
	for !closed && !aborted && !endedNoInput {

		gotFeedback, coalesceFired, keepAliveFired := false, false, false
		attachReq, detachReq, relayDone, gotUserMsg := false, false, false, false
		editDecision := false // this reply decides the fate of hand-edits — ack it specifically, not with the whimsical word
		var relayResult, userMsgText, userMsgAttach string
		sel := workflow.NewSelector(ctx)

		sel.AddReceive(reviewComment, func(c workflow.ReceiveChannel, _ bool) {
			var s ReviewCommentSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			if s.CommentID != 0 && reconcileVersion >= 1 && seenComments[s.CommentID] {
				return // already folded in via the reconcile (or a webhook retry)
			}
			st.PendingInstructions = append(st.PendingInstructions, githubCommentInstruction(s))
			gotFeedback, pendingFromPR = true, true
			if s.CommentID != 0 {
				seenComments[s.CommentID] = true
			}
			if s.CommentID != 0 && s.Path != "" {
				pendingReplyToID = s.CommentID // thread under the inline comment (not a top-level one)
			}
		})
		sel.AddReceive(reviewSubmitted, func(c workflow.ReceiveChannel, _ bool) {
			var s ReviewSubmittedSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			if s.ReviewID != 0 && reconcileVersion >= 1 && seenComments[s.ReviewID] {
				return // already folded in via the reconcile (or a webhook retry)
			}
			if s.ReviewID != 0 {
				seenComments[s.ReviewID] = true
			}
			// Only changes_requested drives another round; approvals/comments don't.
			if s.State == "changes_requested" {
				st.PendingInstructions = append(st.PendingInstructions,
					fmt.Sprintf("[GitHub review by @%s] requested changes: %s", s.Author, s.Body))
				gotFeedback, pendingFromPR = true, true
			}
		})
		sel.AddReceive(ciStatus, func(c workflow.ReceiveChannel, _ bool) {
			var s CIStatusSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			if s.Conclusion == "failure" && in.Policy.AutoFixCI {
				st.PendingInstructions = append(st.PendingInstructions,
					"[GitHub CI] the checks failed. Inspect the failure and fix it: "+s.DetailsURL)
				gotFeedback, pendingFromPR = true, true
			}
		})
		sel.AddReceive(userMessage, func(c workflow.ReceiveChannel, _ bool) {
			var s UserMessageSignal
			c.Receive(ctx, &s)
			gotUserMsg, userMsgText, userMsgAttach = true, s.Text, s.Attachments // routed by intent after Select
		})
		sel.AddReceive(prClosed, func(c workflow.ReceiveChannel, _ bool) {
			var s PRClosedSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			closed, merged = true, s.Merged // merged vs abandoned discriminator
		})
		sel.AddReceive(abort, func(c workflow.ReceiveChannel, _ bool) {
			var s AbortSignal
			c.Receive(ctx, &s)
			aborted, abortReason = true, s.Reason
		})
		sel.AddReceive(attachCh, func(c workflow.ReceiveChannel, _ bool) {
			var s AttachSignal
			c.Receive(ctx, &s)
			attachReq = true
		})
		sel.AddReceive(detachCh, func(c workflow.ReceiveChannel, _ bool) {
			var s DetachSignal
			c.Receive(ctx, &s)
			detachReq = true
		})
		if relay != nil { // stream tunnel output to Slack; resolves when the guest reports detached
			sel.AddFuture(relay, func(f workflow.Future) {
				_ = f.Get(ctx, &relayResult)
				relayDone = true
			})
		}
		sel.AddFuture(ttl, func(workflow.Future) { aborted, abortReason = true, "hard_ttl" })
		if coalesce != nil {
			sel.AddFuture(coalesce, func(workflow.Future) { coalesceFired = true })
		}
		if keepAlive != nil {
			sel.AddFuture(keepAlive, func(workflow.Future) { keepAliveFired = true })
		}

		sel.Select(ctx)

		// Route a natural-language reply by intent — no rigid commands. The `/mando`
		// slash commands remain as an explicit fallback (they arrive on attachCh/detachCh).
		if gotUserMsg && !closed && !aborted {
			// The operator's own words carry the intent; any attached file is content, folded into the
			// instruction but kept out of the intent decision (so a plan that says "attach"/"detach"
			// can't hijack the routing).
			instruction := strings.TrimSpace(userMsgText)
			if userMsgAttach != "" {
				instruction = strings.TrimSpace(instruction + "\n\n" + userMsgAttach)
			}
			if awaitingChoice { // the reply decides the fate of edits made in VS Code — hand it to the agent
				awaitingChoice = false
				st.PendingInstructions = append(st.PendingInstructions, wrapEditDecision(instruction))
				gotFeedback = true
				if editAckVersion >= 1 {
					editDecision = true // ack this specifically below, tied to the operator's own edits
					slackNote(ctx, st, ":inbox_tray: On it — taking care of the changes you made.")
				}
			} else {
				intent := "message"
				if userMsgText != "" { // classify on the comment alone; a file-only drop is just content
					intent = classifyIntent(ctx, in, userMsgText)
				}
				switch intent {
				case "attach":
					if !attached {
						attachReq = true
					}
				case "detach":
					if attached {
						detachReq = true
					}
				default: // a normal instruction — ignore chatter while they're editing in the VM
					if !attached && instruction != "" {
						st.PendingInstructions = append(st.PendingInstructions, instruction)
						gotFeedback = true
					}
				}
			}
		}

		// First feedback of a batch: open the short coalescing window. Ack in Slack only when the
		// feedback came from Slack — a PR comment is answered in its own thread, so a Slack "Looking…"
		// would just be noise the reviewer never asked for.
		if gotFeedback && coalesce == nil && !closed && !aborted && !frozen() {
			if !pendingFromPR && !editDecision {
				ackFeedback(ctx, st, ackN)
				ackN++
			}
			coalesce = workflow.NewTimer(ctx, in.Policy.ReviewDebounce)
		}

		// Coalescing window closed: one turn addresses everything batched — delivered to the
		// warm VM, or a cold resume if it had parked.
		if coalesceFired {
			coalesce = nil
			// Reconcile against GitHub before acting so a dropped webhook can't leave the agent
			// missing part of the conversation — it acts on the complete thread every turn.
			if st.PRNumber > 0 && reconcileVersion >= 1 && !closed && !aborted {
				reconcileThread(ctx, st, seenComments, &pendingFromPR, &pendingReplyToID)
			}
			// While a human is attached (or deciding on their edits), keep feedback queued.
			if len(st.PendingInstructions) > 0 && !closed && !aborted && !frozen() &&
				costCeilingReached(in, st) {
				// Circuit breaker: cumulative spend hit the operator's ceiling. Hold feedback queued and
				// stop running agent turns, so untrusted PR/Slack input can't drive unbounded LLM spend.
				// The session stays alive (PR open) until merged/closed/aborted or its TTL.
				if !st.CostCeilingReached {
					st.CostCeilingReached = true
					slackNote(ctx, st, fmt.Sprintf(":no_entry: Cost ceiling ($%.2f) reached — pausing agent turns. Your feedback stays queued; raise the ceiling (re-dispatch) or close the PR to continue.", in.Policy.CostCeilingUSD))
				}
				st.Phase = "cost_ceiling_reached"
				armKeepAlive()
			} else if len(st.PendingInstructions) > 0 && !closed && !aborted && !frozen() {
				instructions := st.PendingInstructions
				fromPR, replyToID := pendingFromPR, pendingReplyToID
				st.PendingInstructions, pendingFromPR, pendingReplyToID = nil, false, 0
				st.ReviewRound++
				log.Info("review round", "round", st.ReviewRound, "items", len(instructions),
					"warm", st.VMState == vmRunning)
				var r PhaseResult
				if st.VMState == vmRunning {
					r = warmTurn(ctx, in, st, instructions)
				} else {
					r = launchWarm(ctx, in, st, supervisor.ModeResume, instructions)
				}
				recordOutcome(ctx, st, in, r)
				// If the VM dropped before it addressed the feedback, don't lose it — put it back
				// and retry on a fresh cold session next round instead of silently swallowing it.
				if r.Outcome == supervisor.EventAgentFailed {
					st.PendingInstructions = append(instructions, st.PendingInstructions...)
					pendingFromPR = pendingFromPR || fromPR // preserve origin for the retry
					coalesce = workflow.NewTimer(ctx, in.Policy.ReviewDebounce)
					slackNote(ctx, st, ":arrows_counterclockwise: That didn't land (the session dropped) — retrying on a fresh one…")
				} else {
					if fromPR { // feedback came from GitHub — the full reply goes back on the PR thread…
						postPRReply(ctx, st, r.Reply, replyToID)
					}
					reportPhase(ctx, st, r, fromPR) // …and Slack gets a full mirror or a light breadcrumb
				}
				st.Phase = "awaiting_review"
				armKeepAlive()
			}
		}

		// Idle too long: park the warm VM (a later reply cold-resumes it) — unless no PR ever
		// materialized, in which case there is nothing to keep the session for, so end it cleanly.
		if keepAliveFired {
			keepAlive = nil
			switch {
			case st.PRNumber == 0 && len(st.PendingInstructions) == 0:
				endedNoInput = true
				slackNote(ctx, st, ":zzz: No plan came through in time — I've ended this session. Dispatch again whenever you're ready.")
			case st.VMState == vmRunning && len(st.PendingInstructions) == 0:
				teardownVM(ctx, st)
				slackNote(ctx, st, ":zzz: Parked — reply anytime and I'll spin back up.")
			}
		}

		// ---- human attach (VS Code tunnel): pin + pause while an operator is in the VM ----
		if attachReq && !attached && !closed && !aborted {
			attached = true
			if st.VMState != vmRunning { // wake a parked session so there's something to attach to
				r := launchWarm(ctx, in, st, supervisor.ModeResume, nil)
				recordOutcome(ctx, st, in, r)
			}
			slackNote(ctx, st, ":outbox_tray: *Attaching* — I'll drop a VS Code link here in a few seconds; it opens straight in the repo. The agent pauses while you're in — just tell me when you're done.")
			// Subscribe the relay BEFORE telling the guest to start the tunnel — NATS is fire-and-
			// forget, so a relay that subscribes after the login line is published would miss it.
			relay = startRelay(ctx, in, st)
			_ = workflow.Sleep(ctx, 4*time.Second)
			deliver(ctx, in, supervisor.CommandAttach, "", "")
			keepAlive = nil // pinned while attached
		}
		if detachReq && attached && !closed && !aborted {
			deliver(ctx, in, supervisor.CommandDetach, "", "") // guest stops the tunnel + reports the tree
		}
		if relayDone { // the tunnel ended (detach or timeout)
			relay, attached = nil, false
			if strings.TrimSpace(relayResult) == "" { // no hand-edits — nothing to decide
				slackNote(ctx, st, ":inbox_tray: *Detached* — no changes left in the workspace. The agent is active again.")
				armKeepAlive()
				if len(st.PendingInstructions) > 0 && coalesce == nil {
					coalesce = workflow.NewTimer(ctx, in.Policy.ReviewDebounce)
				}
			} else { // ask what to do; stay frozen (agent paused, VM pinned) until they choose
				awaitingChoice = true
				promptDetachChoice(ctx, st, relayResult)
			}
		}

	}

	// ---- teardown ----
	switch {
	case merged:
		st.Phase = "merged"
	case closed:
		st.Phase = "closed"
	case aborted:
		st.Phase = "aborted:" + abortReason
	case endedNoInput:
		st.Phase = "ended_no_input"
	default:
		st.Phase = "ended"
	}
	destroyWorkspace(ctx, st)
	finalSummary(ctx, st, startTime)
	log.Info("workflow complete", "phase", st.Phase, "rounds", st.ReviewRound,
		"cost_usd", st.CumulativeCostUSD, "pr", st.PRNumber)
	return *st, nil
}

// ---- Slack thread rendering. All best-effort: a Slack failure never fails the task. ----

func postRoot(ctx workflow.Context, st *State, in WorkflowInput) {
	var a *Activities
	text := fmt.Sprintf(":robot_face: *Task dispatched* `%s`\n*repo* `%s`  *branch* `%s`\n> %s",
		in.SessionID, in.Repo, st.HeadBranch, truncate(in.Prompt, 300))
	var r PostSlackResult
	if err := workflow.ExecuteActivity(slackCtx(ctx), a.PostSlack,
		PostSlackParams{Channel: in.SlackChannel, Text: text}).Get(ctx, &r); err == nil && r.TS != "" {
		st.SlackThreadTS, st.SlackChannel = r.TS, r.Channel
		// So slack-gateway can route thread replies back to this workflow.
		_ = workflow.UpsertTypedSearchAttributes(ctx,
			temporal.NewSearchAttributeKeyKeyword(SASlackThread).ValueSet(r.TS))
	}
}

// costCeilingReached reports whether cumulative spend has hit the operator's configured ceiling
// (0 disables it). Once true, the workflow stops running agent turns so untrusted PR/Slack input
// cannot drive unbounded LLM spend.
func costCeilingReached(in WorkflowInput, st *State) bool {
	return in.Policy.CostCeilingUSD > 0 && st.CumulativeCostUSD >= in.Policy.CostCeilingUSD
}

func slackNote(ctx workflow.Context, st *State, text string) {
	if st.SlackThreadTS == "" {
		return
	}
	var a *Activities
	_ = workflow.ExecuteActivity(slackCtx(ctx), a.PostSlack,
		PostSlackParams{Channel: st.SlackChannel, ThreadTS: st.SlackThreadTS, Text: text}).Get(ctx, nil)
}

// reportPhase renders a turn as conversation: the agent's own words, plus a small footer noting
// a PR/push when it actually changed code. This is what makes a reply feel answered — a
// question gets an explanation, a change gets a summary — rather than a bare "no changes".
func reportPhase(ctx workflow.Context, st *State, res PhaseResult, fromPR bool) {
	reply := toSlackMrkdwn(strings.TrimSpace(res.Reply)) // agent emits GH-Markdown; Slack needs mrkdwn
	switch res.Outcome {
	case supervisor.EventPROpened:
		msg := fmt.Sprintf(":tada: *PR opened* <%s|#%d>", res.PRURL, res.PRNumber)
		if reply != "" {
			msg += "\n" + truncate(reply, 1500)
		}
		msg += fmt.Sprintf("\n_$%.4f · %d tokens_", res.CostUSD, res.Tokens)
		slackNote(ctx, st, msg)
	case supervisor.EventPushDone:
		// A PR-origin turn already carries the full reply in its PR thread; Slack gets a compact
		// breadcrumb (a short snippet + link) so the timeline stays followable without duplicating a
		// wall of text. Slack-origin turns get the full reply here, since Slack is their only home.
		if fromPR {
			msg := fmt.Sprintf(":left_speech_bubble: Replied on PR <%s|#%d>", st.PRURL, st.PRNumber)
			if reply != "" {
				msg += " — " + truncate(reply, 220)
			}
			if res.Stage != "no_changes" && res.CommitSHA != "" {
				msg += fmt.Sprintf("\n:arrow_up: pushed `%s` · _$%.4f · %d tokens_",
					shortSHA(res.CommitSHA), res.CostUSD, res.Tokens)
			} else {
				msg += fmt.Sprintf(" · _$%.4f · %d tokens_", res.CostUSD, res.Tokens)
			}
			slackNote(ctx, st, msg)
			return
		}
		msg := ":speech_balloon: "
		if reply != "" {
			msg += truncate(reply, 1500)
		} else if res.Stage == "no_changes" {
			msg += "_(no changes)_"
		} else {
			msg += "_(done)_"
		}
		if res.Stage != "no_changes" && res.CommitSHA != "" {
			msg += fmt.Sprintf("\n:arrow_up: pushed `%s` · _$%.4f · %d tokens_",
				shortSHA(res.CommitSHA), res.CostUSD, res.Tokens)
		} else {
			msg += fmt.Sprintf("\n_$%.4f · %d tokens_", res.CostUSD, res.Tokens)
		}
		slackNote(ctx, st, msg)
	case supervisor.EventAgentFailed:
		slackNote(ctx, st, fmt.Sprintf(":x: Run failed at *%s*: %s", res.Stage, res.Error))
	case supervisor.EventNeedsInput:
		slackNote(ctx, st, fmt.Sprintf(":grey_question: *Agent needs input*: %s\n_Reply in this thread to continue._", toSlackMrkdwn(strings.TrimSpace(res.Question))))
	}
}

func finalSummary(ctx workflow.Context, st *State, start time.Time) {
	wall := workflow.Now(ctx).Sub(start).Round(time.Second)
	var b strings.Builder
	fmt.Fprintf(&b, ":checkered_flag: *Session complete* — %s\n", st.Phase)
	if st.PRNumber != 0 {
		fmt.Fprintf(&b, "PR <%s|#%d> · ", st.PRURL, st.PRNumber)
	}
	fmt.Fprintf(&b, "rounds %d · $%.4f · %d tokens · %s",
		st.ReviewRound, st.CumulativeCostUSD, st.CumulativeTokens, wall)
	slackNote(ctx, st, b.String())
}

func slackCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
}

// reconcilePR adopts an open PR that exists on the branch but whose pr_opened event never
// arrived (lost NATS message), so the workflow tracks it instead of tearing down.
func reconcilePR(ctx workflow.Context, st *State) {
	var a *Activities
	var chk CheckPRResult
	if err := workflow.ExecuteActivity(slackCtx(ctx), a.CheckPR,
		CheckPRParams{Repo: st.Repo, Branch: st.HeadBranch}).Get(ctx, &chk); err == nil && chk.Number != 0 {
		st.PRNumber, st.PRURL = chk.Number, chk.URL
		_ = workflow.UpsertTypedSearchAttributes(ctx,
			temporal.NewSearchAttributeKeyInt64(SAPRNumber).ValueSet(int64(chk.Number)))
		slackNote(ctx, st, fmt.Sprintf(":mag: Recovered PR <%s|#%d> (its open event was lost in transit).",
			chk.URL, chk.Number))
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// launchWarm mints → launches (cold) → runs the first turn, and leaves the VM WARM. The
// guest stays up handling follow-ups until it idles out or the workflow tears it down.
func launchWarm(ctx workflow.Context, in WorkflowInput, st *State, mode string, instructions []string) PhaseResult {
	var a *Activities // nil receiver: used only for type-safe activity references

	st.Phase = "minting:" + mode
	var creds Credentials
	if err := workflow.ExecuteActivity(mintCtx(ctx), a.MintCredentials, in).Get(ctx, &creds); err != nil {
		return PhaseResult{Outcome: supervisor.EventAgentFailed, Stage: "mint", Error: err.Error()}
	}

	st.Phase = "launching:" + mode
	var lr LaunchResult
	lp := LaunchParams{Input: in, Creds: creds, Mode: mode, Instructions: instructions, HeadBranch: st.HeadBranch}
	if err := workflow.ExecuteActivity(launchCtx(ctx), a.LaunchVM, lp).Get(ctx, &lr); err != nil {
		return PhaseResult{Outcome: supervisor.EventAgentFailed, Stage: "launch", Error: err.Error()}
	}
	st.VMState = vmRunning

	st.Phase = "running:" + mode
	res := awaitTurn(ctx, in)
	settle(ctx, st, res)
	return res
}

// warmTurn delivers the batched messages to the already-running guest and awaits the resulting
// turn — no relaunch, no checkout, no debounce (keep-alive).
func warmTurn(ctx workflow.Context, in WorkflowInput, st *State, texts []string) PhaseResult {
	for _, t := range texts {
		deliver(ctx, in, supervisor.CommandUserMessage, t, "")
	}
	st.Phase = "running:warm"
	res := awaitTurn(ctx, in)
	settle(ctx, st, res)
	return res
}

// awaitTurn blocks on RunAgentPhase for one guest turn's terminal event.
func awaitTurn(ctx workflow.Context, in WorkflowInput) PhaseResult {
	var a *Activities
	var res PhaseResult
	if err := workflow.ExecuteActivity(phaseCtx(ctx), a.RunAgentPhase, in.SessionID).Get(ctx, &res); err != nil {
		return PhaseResult{Outcome: supervisor.EventAgentFailed, Stage: "run", Error: err.Error()}
	}
	return res
}

// settle records the VM's warmth after a turn: still warm after a push/PR, torn down otherwise
// (a failed or lost guest leaves nothing to talk to).
func settle(ctx workflow.Context, st *State, res PhaseResult) {
	switch res.Outcome {
	case supervisor.EventPROpened, supervisor.EventPushDone:
		st.VMState = vmRunning
	default:
		teardownVM(ctx, st)
	}
}

// teardownVM stops the microVM but keeps the workspace, so the session can be resumed cold.
func teardownVM(ctx workflow.Context, st *State) {
	var a *Activities
	_ = workflow.ExecuteActivity(destroyCtx(ctx), a.DestroyVM,
		DestroyParams{SessionID: st.SessionID}).Get(ctx, nil)
	st.VMState = vmDestroyed
}

// destroyWorkspace discards the persistent volume at end-of-life.
func destroyWorkspace(ctx workflow.Context, st *State) {
	var a *Activities
	_ = workflow.ExecuteActivity(destroyCtx(ctx), a.DestroyVM,
		DestroyParams{SessionID: st.SessionID, PurgeWorkspace: true}).Get(ctx, nil)
	st.VMState = vmDestroyed
}

func deliver(ctx workflow.Context, in WorkflowInput, typ, text, reason string) {
	var a *Activities
	_ = workflow.ExecuteActivity(deliverCtx(ctx), a.DeliverMessage,
		DeliverParams{SessionID: in.SessionID, Type: typ, Text: text, Reason: reason}).Get(ctx, nil)
}

// ackWords are whimsical present-participles (à la Claude Code's spinner) so the instant "I heard
// you" reads with a bit of life instead of the same "Looking…" on every reply. Rotated by a
// counter — deterministic, which the workflow requires (no rand).
var ackWords = []string{
	"Swirling", "Percolating", "Noodling", "Cogitating", "Puzzling", "Simmering",
	"Brewing", "Pondering", "Marinating", "Ruminating", "Conjuring", "Churning",
	"Mulling", "Wrangling", "Tinkering", "Whirring", "Musing", "Spelunking",
	"Untangling", "Vibing",
}

var ackEmoji = []string{
	":cyclone:", ":sparkles:", ":thought_balloon:", ":crystal_ball:",
	":gear:", ":brain:", ":ocean:", ":hammer_and_wrench:",
}

// ackFeedback posts the instant, state-aware "I heard you" so a spin-up delay never looks like
// silence. It doesn't presume a code change (the reply may just be a question); the agent's
// actual response follows. n rotates the flavor word so it isn't the same phrase every time.
func ackFeedback(ctx workflow.Context, st *State, n int) {
	msg := fmt.Sprintf("%s %s…", ackEmoji[n%len(ackEmoji)], ackWords[n%len(ackWords)])
	if st.VMState != vmRunning {
		msg += " _(waking a session, ~1 min)_"
	}
	slackNote(ctx, st, msg)
}

// recordOutcome folds a phase result into workflow state and search attributes.
func recordOutcome(ctx workflow.Context, st *State, in WorkflowInput, res PhaseResult) {
	st.CumulativeCostUSD += res.CostUSD
	st.CumulativeTokens += res.Tokens
	if res.Outcome == supervisor.EventPROpened && res.PRNumber != 0 {
		st.PRNumber = res.PRNumber
		st.PRURL = res.PRURL
	}
	if res.CommitSHA != "" {
		// nothing to store beyond state; kept for future push tracking
	}
	upd := []temporal.SearchAttributeUpdate{
		temporal.NewSearchAttributeKeyInt64(SAReviewRound).ValueSet(int64(st.ReviewRound)),
	}
	if st.PRNumber != 0 {
		upd = append(upd, temporal.NewSearchAttributeKeyInt64(SAPRNumber).ValueSet(int64(st.PRNumber)))
	}
	_ = workflow.UpsertTypedSearchAttributes(ctx, upd...)
}

// githubCommentInstruction attributes a PR review comment so the agent knows it's from GitHub and
// where — the file/line for an inline comment — giving it the same context a human reviewer has.
func githubCommentInstruction(s ReviewCommentSignal) string {
	where := ""
	if s.Path != "" {
		where = fmt.Sprintf(" on `%s`", s.Path)
		if s.Line > 0 {
			where = fmt.Sprintf(" on `%s:%d`", s.Path, s.Line)
		}
	}
	return fmt.Sprintf("[GitHub review by @%s%s] %s", s.Author, where, s.Body)
}

// reconcileThread pulls the PR's full conversation from GitHub and folds in any human comment the
// workflow hasn't delivered yet — the safety net for a dropped webhook, so the agent acts on the
// complete thread every turn. Best-effort: a GitHub failure just leaves webhooks as the
// only delivery path. Marks each folded-in item seen and points the reply at the newest inline
// comment being caught up on.
func reconcileThread(ctx workflow.Context, st *State, seen map[int64]bool, fromPR *bool, replyToID *int64) {
	if st.PRNumber == 0 {
		return
	}
	var a *Activities
	var thread []ThreadComment
	if err := workflow.ExecuteActivity(slackCtx(ctx), a.FetchPRThread,
		FetchThreadParams{Repo: st.Repo, PRNumber: st.PRNumber}).Get(ctx, &thread); err != nil {
		return
	}
	for _, c := range thread {
		if c.ID == 0 || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		// A review summary that isn't changes_requested (an approval, or the container for inline
		// comments already fetched) isn't actionable feedback — record it seen but don't act on it.
		if c.Kind == "review" && c.State != "changes_requested" {
			continue
		}
		st.PendingInstructions = append(st.PendingInstructions, threadInstruction(c))
		*fromPR = true
		if c.Kind == "review_comment" {
			*replyToID = c.ID
		}
	}
}

// threadInstruction renders a reconciled comment the same way githubCommentInstruction renders a
// live one, so the agent can't tell (and needn't) whether a comment arrived by webhook or reconcile.
func threadInstruction(c ThreadComment) string {
	switch c.Kind {
	case "review_comment":
		where := ""
		if c.Path != "" {
			where = fmt.Sprintf(" on `%s`", c.Path)
			if c.Line > 0 {
				where = fmt.Sprintf(" on `%s:%d`", c.Path, c.Line)
			}
		}
		return fmt.Sprintf("[GitHub review by @%s%s] %s", c.Author, where, c.Body)
	case "review":
		return fmt.Sprintf("[GitHub review by @%s] requested changes: %s", c.Author, c.Body)
	default: // issue_comment
		return fmt.Sprintf("[GitHub comment by @%s] %s", c.Author, c.Body)
	}
}

// postPRReply mirrors the agent's reply into the PR so a GitHub reviewer sees the response in
// place (not only in Slack). Best-effort — a GitHub failure never fails the task.
func postPRReply(ctx workflow.Context, st *State, reply string, replyToID int64) {
	reply = strings.TrimSpace(reply)
	if reply == "" || st.PRNumber == 0 {
		return
	}
	var a *Activities
	_ = workflow.ExecuteActivity(slackCtx(ctx), a.PostPRComment,
		PostPRCommentParams{Repo: st.Repo, PRNumber: st.PRNumber, Body: reply, ReplyToID: replyToID}).Get(ctx, nil)
}

// startRelay launches RelayTunnel, which streams the human-attach tunnel's output to the Slack
// thread and resolves when the guest reports it detached.
func startRelay(ctx workflow.Context, in WorkflowInput, st *State) workflow.Future {
	var a *Activities
	return workflow.ExecuteActivity(relayCtx(ctx), a.RelayTunnel,
		RelayParams{SessionID: in.SessionID, Channel: st.SlackChannel, ThreadTS: st.SlackThreadTS})
}

// promptDetachChoice shows the operator what they left in the working tree and asks, in plain
// language, what to do with it. Their reply is handed to the agent, which interprets and acts.
func promptDetachChoice(ctx workflow.Context, st *State, treeStatus string) {
	slackNote(ctx, st, fmt.Sprintf(":inbox_tray: *Detached.* You left changes in the workspace:\n```\n%s\n```\nJust tell me what to do with them — e.g. \"commit those\", \"drop them\", or \"finish what I started\".",
		truncate(treeStatus, 1000)))
}

// wrapEditDecision frames the operator's reply for the agent, so it can act on hand-edits the human
// left during a VS Code attach (keep/build → committed automatically; discard → git checkout/clean).
func wrapEditDecision(reply string) string {
	return fmt.Sprintf("You just finished a hands-on VS Code session in this workspace and left "+
		"uncommitted changes there. The operator now says: %q. Do what they mean: keep or build on "+
		"those changes (they are committed automatically when you finish), or discard them by running "+
		"`git checkout -- . && git clean -fd` if that's what they want. Then confirm what you did.", reply)
}

// classifyIntent asks the cheap model whether a reply means "get me into the VM", "I'm done in the
// VM", or a normal instruction — so plain phrasing works without a command. Fails safe to "message".
func classifyIntent(ctx workflow.Context, in WorkflowInput, msg string) string {
	var a *Activities
	var intent string
	if err := workflow.ExecuteActivity(mintCtx(ctx), a.ClassifyIntent, msg).Get(ctx, &intent); err != nil {
		return "message"
	}
	return intent
}

// ---- per-activity option contexts (retry + timeout tuned per activity) ----

func mintCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
}

func launchCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 120 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: 5 * time.Second, BackoffCoefficient: 2, MaximumAttempts: 5},
	})
}

func phaseCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 6 * time.Hour,
		HeartbeatTimeout:    5 * time.Minute,                           // margin over the SDK's throttled heartbeats
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1}, // a run is not idempotent
	})
}

func destroyCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 120 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	})
}

func deliverCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 15 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
}

// relayCtx bounds a human-attach tunnel relay — long-lived (the max attach length), single attempt.
func relayCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    6 * time.Hour,   // a single attach can run long
		ScheduleToCloseTimeout: 6 * time.Hour,   // bound the whole attach, retries included
		HeartbeatTimeout:       2 * time.Minute, // a dead/stalled relay is detected within 2m
		// Retry across worker restarts: a redeploy cancels the relay ("context canceled"); a fresh
		// attempt re-subscribes and recovers the tunnel URL (the guest re-broadcasts it for the whole
		// attach), so an attach in progress is never silently stranded without its link.
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second,
			MaximumInterval: 15 * time.Second,
			MaximumAttempts: 0, // unlimited within the ScheduleToClose window
		},
	})
}
