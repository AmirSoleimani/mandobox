package control

import (
	"fmt"
	"strings"
	"time"

	"github.com/acme/fleet/internal/supervisor"
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

	// Initial phase: mint → launch → run → destroy (keep workspace).
	res := runPhase(ctx, in, st, supervisor.ModeInitial, nil)
	recordOutcome(ctx, st, in, res)
	reportPhase(ctx, st, res)

	// If the first run never opened a PR, there is nothing to review — tear down and finish.
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
		debounce                workflow.Future // armed on the first pending instruction
	)
	arm := func() {
		if debounce == nil {
			debounce = workflow.NewTimer(ctx, in.Policy.ReviewDebounce)
		}
	}

	st.Phase = "awaiting_review"
	for !closed && !aborted &&
		st.ReviewRound < in.Policy.MaxReviewRounds &&
		st.CumulativeCostUSD < in.Policy.CostCeilingUSD {

		sel := workflow.NewSelector(ctx)

		sel.AddReceive(reviewComment, func(c workflow.ReceiveChannel, _ bool) {
			var s ReviewCommentSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			st.PendingInstructions = append(st.PendingInstructions, instructionFromComment(s.Author, s.Body))
			arm()
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
				arm()
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
				arm()
			}
		})
		sel.AddReceive(prClosed, func(c workflow.ReceiveChannel, _ bool) {
			var s PRClosedSignal
			c.Receive(ctx, &s)
			if dedupe(s.DeliveryID) {
				return
			}
			closed, merged = true, s.Merged // merged vs abandoned discriminator (§6.2)
		})
		sel.AddReceive(userMessage, func(c workflow.ReceiveChannel, _ bool) {
			var s UserMessageSignal
			c.Receive(ctx, &s)
			if st.VMState == vmRunning {
				deliver(ctx, in, st, supervisor.CommandUserMessage, s.Text, "")
			} else {
				st.PendingInstructions = append(st.PendingInstructions, s.Text)
				arm()
			}
		})
		sel.AddReceive(abort, func(c workflow.ReceiveChannel, _ bool) {
			var s AbortSignal
			c.Receive(ctx, &s)
			aborted, abortReason = true, s.Reason
		})
		sel.AddFuture(ttl, func(workflow.Future) {
			aborted, abortReason = true, "hard_ttl"
		})
		if debounce != nil {
			sel.AddFuture(debounce, func(workflow.Future) { debounce = nil })
		}

		sel.Select(ctx)

		// A fired debounce with pending work → run one resume round batching all of it.
		if debounce == nil && len(st.PendingInstructions) > 0 && !closed && !aborted {
			instructions := st.PendingInstructions
			st.PendingInstructions = nil
			st.ReviewRound++
			log.Info("resume round", "round", st.ReviewRound, "instructions", len(instructions))
			slackNote(ctx, st, fmt.Sprintf(":arrows_counterclockwise: *Review round %d* — addressing %d item(s).",
				st.ReviewRound, len(instructions)))
			r := runPhase(ctx, in, st, supervisor.ModeResume, instructions)
			recordOutcome(ctx, st, in, r)
			reportPhase(ctx, st, r)
			st.Phase = "awaiting_review"
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

func reportPhase(ctx workflow.Context, st *State, res PhaseResult) {
	switch res.Outcome {
	case supervisor.EventPROpened:
		slackNote(ctx, st, fmt.Sprintf(":tada: *PR opened* <%s|#%d>  (cost $%.4f · %d tokens)",
			res.PRURL, res.PRNumber, res.CostUSD, res.Tokens))
	case supervisor.EventPushDone:
		if res.Stage == "no_changes" {
			slackNote(ctx, st, ":information_source: Agent produced no changes this round.")
		} else {
			slackNote(ctx, st, fmt.Sprintf(":arrow_up: Pushed `%s`  (cost $%.4f · %d tokens)",
				shortSHA(res.CommitSHA), res.CostUSD, res.Tokens))
		}
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

// runPhase does mint → launch → run → destroy-vm(keep workspace) for one mode.
func runPhase(ctx workflow.Context, in WorkflowInput, st *State, mode string, instructions []string) PhaseResult {
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
	var res PhaseResult
	if err := workflow.ExecuteActivity(phaseCtx(ctx), a.RunAgentPhase, in.SessionID).Get(ctx, &res); err != nil {
		res = PhaseResult{Outcome: supervisor.EventAgentFailed, Stage: "run", Error: err.Error()}
	}

	st.Phase = "destroying:" + mode
	_ = workflow.ExecuteActivity(destroyCtx(ctx), a.DestroyVM, DestroyParams{SessionID: in.SessionID}).Get(ctx, nil)
	st.VMState = vmDestroyed
	return res
}

// destroyWorkspace discards the persistent volume at end-of-life (§7.6).
func destroyWorkspace(ctx workflow.Context, st *State) {
	var a *Activities
	_ = workflow.ExecuteActivity(destroyCtx(ctx), a.DestroyVM,
		DestroyParams{SessionID: st.SessionID, PurgeWorkspace: true}).Get(ctx, nil)
	st.VMState = vmDestroyed
}

func deliver(ctx workflow.Context, in WorkflowInput, st *State, typ, text, reason string) {
	var a *Activities
	_ = workflow.ExecuteActivity(deliverCtx(ctx), a.DeliverMessage,
		DeliverParams{SessionID: in.SessionID, Type: typ, Text: text, Reason: reason}).Get(ctx, nil)
	_ = st
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
		HeartbeatTimeout:    90 * time.Second,
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
