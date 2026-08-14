package control

import (
	"context"
	"strings"

	"github.com/AmirSoleimani/mandobox/internal/llm"
)

const classifySystem = `You route messages sent to an AI coding assistant that is working on a pull ` +
	`request inside an isolated dev VM. Classify the user's message into exactly one word:
- ATTACH — the user wants to personally get into the dev environment to look at or edit the code by ` +
	`hand (e.g. "let me in", "can I get in", "I want to make the change myself", "give me a vscode ` +
	`link", "let me poke at it", "I'll edit it").
- DETACH — the user is finished in the environment and wants to close or leave it (e.g. "I'm done", ` +
	`"close it", "you can take it back now").
- MESSAGE — anything else: a coding request, a question, feedback, or discussion.
Reply with ONLY one word: ATTACH, DETACH, or MESSAGE.`

const planDecisionSystem = `An AI coding assistant has proposed a PLAN for a task and is discussing it ` +
	`with a human reviewer before implementing anything. Classify the reviewer's latest message into ` +
	`exactly one word:
- PROCEED — the reviewer approves the plan and wants the assistant to start implementing it NOW (e.g. ` +
	`"go", "looks good", "lgtm", "ship it", "yes do it", "sounds good, build it", "go ahead and implement").
- DISCUSS — anything else: a question, a requested change or refinement to the plan, a concern, a request ` +
	`for more detail, or general discussion. This means keep planning — do NOT start building yet.
When in doubt, choose DISCUSS: never start building on an ambiguous message.
Reply with ONLY one word: PROCEED or DISCUSS.`

// ClassifyIntent decides whether a natural-language reply is a request to get into the VM (ATTACH),
// to leave it (DETACH), or a normal instruction (MESSAGE). Fail-safe: any error classifies as
// "message" so the reply still reaches the agent.
func (a *Activities) ClassifyIntent(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "message", nil
	}
	t := a.helperClassify(ctx, classifySystem, message)
	switch {
	case strings.Contains(t, "attach"):
		return "attach", nil
	case strings.Contains(t, "detach"):
		return "detach", nil
	default:
		return "message", nil
	}
}

// ClassifyPlanDecision decides whether the reviewer's reply during plan discussion means "start building"
// (PROCEED) or "keep discussing / refine the plan" (DISCUSS). Fail-safe to "discuss": an ambiguous reply —
// or any error — must never auto-launch a build. See docs on plan mode.
func (a *Activities) ClassifyPlanDecision(ctx context.Context, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "discuss", nil
	}
	return planDecisionFromText(a.helperClassify(ctx, planDecisionSystem, message)), nil
}

// planDecisionFromText maps the model's reply to the decision. Fail-safe: only a reply whose FIRST word
// is "proceed" starts a build; every other reply (including a negation like "do not proceed", and anything
// unrecognized or empty) stays in discussion. First-word (not substring) so the fail-safe can't open on a
// negated "proceed" — the system prompt already asks for a single word.
func planDecisionFromText(t string) string {
	if fields := strings.Fields(strings.ToLower(t)); len(fields) > 0 && strings.HasPrefix(fields[0], "proceed") {
		return "proceed"
	}
	return "discuss"
}

// helperClassify sends a single-word classification request to the active provider's cheap model and
// returns the model's lowercased text reply, or "" on any error (callers apply their own fail-safe
// default). Route: subscription → Anthropic directly on the OAuth token; API-key providers → the gateway
// (which injects the real key, so the placeholder bearer is fine) — the same path the agent uses, so no
// extra secret is needed. Unexported so it is not mistaken for a Temporal activity (see register_test.go).
func (a *Activities) helperClassify(ctx context.Context, system, message string) string {
	baseURL, token, model := a.resolveProvider().helperLLM(a.GatewayURL)
	c := llm.New(baseURL, token, model)
	c.MaxTokens = 5 // these classifiers reply with a single word
	return c.Classify(ctx, system, message)
}
