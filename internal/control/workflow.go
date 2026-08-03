package control

import (
	"fmt"
	"strings"
	"time"

	"github.com/chelodo/fleet/internal/supervisor"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// maxSeenDeliveries bounds the dedupe set kept in workflow state (§6.2). GitHub redelivers,
// so we drop already-seen delivery IDs; the set is trimmed FIFO to keep history small.
const maxSeenDeliveries = 500

// PRWorkflow owns one task from dispatch through the human review loop to teardown (§6.1).
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

	startTime := workflow.Now(ctx)
	postRoot(ctx, st, in) // opens the Slack thread; sets SlackThreadTS + SlackChannel

	// Hard TTL bounds the whole workflow (D2: 24h while iterating).
	ttl := workflow.NewTimer(ctx, in.Policy.HardTTL)

	// Initial phase: mint → launch → run, and keep the VM warm for the review that follows.
	res := launchWarm(ctx, in, st, supervisor.ModeInitial, nil)
	recordOutcome(ctx, st, in, res)
	reportPhase(ctx, st, res)

	// The pr_opened event travels over NATS (at-most-once); if it was lost — e.g. the run went
	// quiet and RunAgentPhase gave up while the guest went on to open the PR — reconcile with
	// GitHub so a real PR is never mistaken for "no PR" (§6).
	if st.PRNumber == 0 {
		reconcilePR(ctx, st)
	}

	// If the first run genuinely opened no PR, there is nothing to review — tear down and finish.
	if st.PRNumber == 0 {
		st.Phase = "no_pr"
		destroyWorkspace(ctx, st)
		finalSummary(ctx, st, startTime)
		return *st, nil
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

	var (
		closed, merged, aborted bool
		abortReason             string
		coalesce                workflow.Future // short window batching a burst into one turn
		keepAlive               workflow.Future // warmth timer; firing parks the idle VM
	)
	armKeepAlive := func() {
		if st.VMState == vmRunning {
			keepAlive = workflow.NewTimer(ctx, in.Policy.KeepAlive)
		} else {
			keepAlive = nil
		}
	}

	st.Phase = "awaiting_review"
	armKeepAlive()
	slackNote(ctx, st, ":speech_balloon: Session's warm — reply in this thread with any changes and I'll jump on them.")
	for !closed && !aborted &&
		st.ReviewRound < in.Policy.MaxReviewRounds &&
		st.CumulativeCostUSD < in.Policy.CostCeilingUSD {

		gotFeedback, coalesceFired, keepAliveFired := false, false, false
		sel := workflow.NewSelector(ctx)

		sel.AddReceive(reviewComment, func(c workflow.ReceiveChannel, _ bool) {
			var s ReviewCommentSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			st.PendingInstructions = append(st.PendingInstructions, instructionFromComment(s.Author, s.Body))
			gotFeedback = true
		})
		sel.AddReceive(reviewSubmitted, func(c workflow.ReceiveChannel, _ bool) {
			var s ReviewSubmittedSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			// Only changes_requested drives another round; approvals/comments don't (§6.1).
			if s.State == "changes_requested" {
				st.PendingInstructions = append(st.PendingInstructions,
					instructionFromComment(s.Author, "Requested changes: "+s.Body))
				gotFeedback = true
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
					"CI failed. Inspect the failure and fix it: "+s.DetailsURL)
				gotFeedback = true
			}
		})
		sel.AddReceive(userMessage, func(c workflow.ReceiveChannel, _ bool) {
			var s UserMessageSignal
			c.Receive(ctx, &s)
			st.PendingInstructions = append(st.PendingInstructions, s.Text)
			gotFeedback = true
		})
		sel.AddReceive(prClosed, func(c workflow.ReceiveChannel, _ bool) {
			var s PRClosedSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			closed, merged = true, s.Merged // merged vs abandoned discriminator (§6.2)
		})
		sel.AddReceive(abort, func(c workflow.ReceiveChannel, _ bool) {
			var s AbortSignal
			c.Receive(ctx, &s)
			aborted, abortReason = true, s.Reason
		})
		sel.AddFuture(ttl, func(workflow.Future) { aborted, abortReason = true, "hard_ttl" })
		if coalesce != nil {
			sel.AddFuture(coalesce, func(workflow.Future) { coalesceFired = true })
		}
		if keepAlive != nil {
			sel.AddFuture(keepAlive, func(workflow.Future) { keepAliveFired = true })
		}

		sel.Select(ctx)

		// First feedback of a batch: ack instantly and open the short coalescing window.
		if gotFeedback && coalesce == nil && !closed && !aborted {
			ackFeedback(ctx, st)
			coalesce = workflow.NewTimer(ctx, in.Policy.ReviewDebounce)
		}

		// Coalescing window closed: one turn addresses everything batched — delivered to the
		// warm VM, or a cold resume if it had parked.
		if coalesceFired {
			coalesce = nil
			if len(st.PendingInstructions) > 0 && !closed && !aborted {
				instructions := st.PendingInstructions
				st.PendingInstructions = nil
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
				reportPhase(ctx, st, r)
				st.Phase = "awaiting_review"
				armKeepAlive()
			}
		}

		// Idle too long: park the warm VM. A later reply cold-resumes it.
		if keepAliveFired {
			keepAlive = nil
			if st.VMState == vmRunning && len(st.PendingInstructions) == 0 {
				teardownVM(ctx, st)
				slackNote(ctx, st, ":zzz: Parked — reply anytime and I'll spin back up.")
			}
		}
	}

	// ---- teardown ----
	if aborted {
		st.Phase = "aborted:" + abortReason
	} else if merged {
		st.Phase = "merged"
	} else if closed {
		st.Phase = "closed"
	} else {
		st.Phase = "review_budget_exhausted"
	}
	destroyWorkspace(ctx, st)
	finalSummary(ctx, st, startTime)
	log.Info("workflow complete", "phase", st.Phase, "rounds", st.ReviewRound,
		"cost_usd", st.CumulativeCostUSD, "pr", st.PRNumber)
	return *st, nil
}

// ---- Slack thread rendering (§6.4). All best-effort: a Slack failure never fails the task. ----

func postRoot(ctx workflow.Context, st *State, in WorkflowInput) {
	var a *Activities
	text := fmt.Sprintf(":robot_face: *Task dispatched* `%s`\n*repo* `%s`  *branch* `%s`\n> %s",
		in.SessionID, in.Repo, st.HeadBranch, truncate(in.Prompt, 300))
	var r PostSlackResult
	if err := workflow.ExecuteActivity(slackCtx(ctx), a.PostSlack,
		PostSlackParams{Channel: in.SlackChannel, Text: text}).Get(ctx, &r); err == nil && r.TS != "" {
		st.SlackThreadTS, st.SlackChannel = r.TS, r.Channel
		// So slack-gateway can route thread replies back to this workflow (§6.4).
		_ = workflow.UpsertTypedSearchAttributes(ctx,
			temporal.NewSearchAttributeKeyKeyword(SASlackThread).ValueSet(r.TS))
	}
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
// a PR/push when it actually changed code (§6.4). This is what makes a reply feel answered — a
// question gets an explanation, a change gets a summary — rather than a bare "no changes".
func reportPhase(ctx workflow.Context, st *State, res PhaseResult) {
	reply := strings.TrimSpace(res.Reply)
	switch res.Outcome {
	case supervisor.EventPROpened:
		msg := fmt.Sprintf(":tada: *PR opened* <%s|#%d>", res.PRURL, res.PRNumber)
		if reply != "" {
			msg += "\n" + truncate(reply, 1500)
		}
		msg += fmt.Sprintf("\n_$%.4f · %d tokens_", res.CostUSD, res.Tokens)
		slackNote(ctx, st, msg)
	case supervisor.EventPushDone:
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
		slackNote(ctx, st, fmt.Sprintf(":grey_question: *Agent needs input*: %s\n_Reply in this thread to continue._", res.Question))
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

// launchWarm mints → launches (cold) → runs the first turn, and leaves the VM WARM (§6.1). The
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
	lp := LaunchParams{Input: in, Creds: creds, Mode: mode, Instructions: instructions}
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
// turn — no relaunch, no checkout, no debounce (§6.1 keep-alive).
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

// destroyWorkspace discards the persistent volume at end-of-life (§7.6).
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

// ackFeedback posts the instant, state-aware "I heard you" so a spin-up delay never looks like
// silence (§6.4).
// ackFeedback is the neutral "I heard you" — it doesn't presume a code change (the reply may
// just be a question); the agent's actual response follows.
func ackFeedback(ctx workflow.Context, st *State) {
	if st.VMState == vmRunning {
		slackNote(ctx, st, ":thought_balloon: Looking…")
	} else {
		slackNote(ctx, st, ":thought_balloon: Looking… _(spinning a session up, ~1 min)_")
	}
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

func instructionFromComment(author, body string) string {
	if author != "" {
		return "@" + author + ": " + body
	}
	return body
}

// ---- per-activity option contexts (retry + timeout tuned per PLAN §6.1) ----

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
		HeartbeatTimeout:    5 * time.Minute, // margin over the SDK's throttled heartbeats
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
