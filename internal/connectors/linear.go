package connectors

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/control"
	"github.com/AmirSoleimani/mandobox/internal/linear"
	"github.com/AmirSoleimani/mandobox/internal/llm"
)

// linearTriggerLabel is the issue label that hands work to the agent.
const linearTriggerLabel = "mando"

// linearConnector is the Linear chat connector's inbound half: a signature-verified webhook server. When
// an issue gains the `mando` label (and is still in a to-do state) it infers the repo from the issue text
// with a cheap LLM (grounded to an allowlist), dispatches a PRWorkflow, and moves the issue to In Progress.
// A comment on the issue steers the run. The outbound half (comments + state moves) is control.linearNotifier.
type linearConnector struct {
	apiKey        string
	webhookSecret string
	webhookAddr   string
	allowlist     []string // OPTIONAL constraint: if set, the LLM must pick one of these; empty → free-form inference
	defaultRepo   string   // fallback when the repo can't be inferred (optional)
	model         string   // OPTIONAL cheap-model override (LINEAR_REPO_MODEL); else the active provider's cheap model
	gatewayURL    string
	providerCfg   string // provider.json — resolve the same LLM route as the worker (subscription vs API-key)
	oauthToken    string // claude-oauth-token path (used on a subscription box)
	authMode      string // legacy agent-auth toggle path (fallback when provider.json is absent)

	client    *linear.Client
	llm       *llm.Client
	viewerID  string // the bot's own user id, to suppress our own comments
	dedupe    *lruSet
	issueLock *keyedMutex // serializes same-issue processing in-process (closes the double-dispatch race)
}

func newLinear() Connector {
	return &linearConnector{
		apiKey:        os.Getenv("LINEAR_API_KEY"),
		webhookSecret: os.Getenv("LINEAR_WEBHOOK_SECRET"),
		webhookAddr:   envOr("LINEAR_WEBHOOK_ADDR", "0.0.0.0:8089"),
		allowlist:     splitRepos(os.Getenv("LINEAR_REPO_ALLOWLIST")),
		defaultRepo:   strings.TrimSpace(os.Getenv("LINEAR_DEFAULT_REPO")),
		model:         strings.TrimSpace(os.Getenv("LINEAR_REPO_MODEL")), // optional override; else provider cheap model
		gatewayURL:    os.Getenv("GATEWAY_URL"),
		providerCfg:   envOr("MANDO_PROVIDER_CONFIG", "/etc/fleet/provider.json"),
		oauthToken:    envOr("MANDO_CLAUDE_OAUTH_TOKEN", "/etc/fleet/claude-oauth-token"),
		authMode:      envOr("MANDO_AGENT_AUTH", "/etc/fleet/agent-auth"),
		dedupe:        newLRUSet(500),
		issueLock:     newKeyedMutex(),
	}
}

func (l *linearConnector) Kind() string     { return "linear" }
func (l *linearConnector) Configured() bool { return l.apiKey != "" && l.webhookSecret != "" }

func (l *linearConnector) Notifier() control.Notifier {
	if l.apiKey == "" {
		return nil
	}
	return control.NewLinearNotifier(l.apiKey)
}

func (l *linearConnector) Serve(ctx context.Context, d *Dispatcher) error {
	l.client = linear.New(l.apiKey)
	// Resolve the cheap-model route the same way the worker does, so it follows the active provider:
	// subscription → Anthropic direct on the OAuth token; API-key → the gateway (which injects the real
	// key). A hardcoded gateway route silently fails on a subscription box, where LiteLLM holds no key.
	baseURL, token, model := control.HelperLLMFromPaths(l.providerCfg, l.oauthToken, l.authMode, l.gatewayURL)
	if l.model != "" {
		model = l.model // LINEAR_REPO_MODEL override
	}
	l.llm = llm.New(baseURL, token, model)
	l.llm.MaxTokens = 24 // enough for an owner/name slug or UNRESOLVED
	log.Printf("connectors/linear: repo inference model=%q via %s", model, baseURL)

	// Resolve the bot's viewer id so we can tell our own comments apart. Best-effort with a few retries; if
	// it never resolves we still serve issue dispatch but drop ALL comment events (fail-closed against an
	// echo loop — see handleComment).
	for attempt := 0; attempt < 3 && l.viewerID == ""; attempt++ {
		if id, err := l.client.Viewer(ctx); err == nil {
			l.viewerID = id
			break
		} else {
			log.Printf("connectors/linear viewer (attempt %d): %v", attempt+1, err)
			sleepCtx(ctx, 3*time.Second)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	if l.viewerID == "" {
		log.Printf("connectors/linear: could not resolve viewer id — comment steering disabled (fail-closed)")
	} else {
		log.Printf("connectors/linear: authenticated as viewer %s", l.viewerID)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/linear", func(w http.ResponseWriter, r *http.Request) { l.handle(ctx, d, w, r) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := &http.Server{Addr: l.webhookAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("connectors/linear: webhook listening on %s (POST /linear)", l.webhookAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return ctx.Err()
}

func (l *linearConnector) handle(ctx context.Context, d *Dispatcher, w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if !linear.VerifySignature([]byte(l.webhookSecret), body, r.Header.Get("Linear-Signature")) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ev, err := linear.ParseEvent(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !linear.FreshTimestamp(ev.WebhookTimestamp, time.Now(), 60*time.Second) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// ACK immediately, then process async: a slow LLM call must not make Linear retry the delivery. Use the
	// long-lived Serve ctx (not the request ctx, which is cancelled once this handler returns).
	w.WriteHeader(http.StatusOK)
	log.Printf("connectors/linear: received %s/%s", ev.Type, ev.Action)
	switch ev.Type {
	case "Issue":
		go l.handleIssue(ctx, d, ev)
	case "Comment":
		go l.handleComment(ctx, d, ev)
	}
}

func (l *linearConnector) handleIssue(ctx context.Context, d *Dispatcher, ev linear.Event) {
	id := ev.IssueID()
	// Dedupe exact redeliveries (same delivery timestamp) but let a genuine re-label (new timestamp)
	// re-trigger — the pickup guards make a re-attempt idempotent.
	if id == "" || l.dedupe.seen(fmt.Sprintf("issue:%s:%s:%d", id, ev.Action, ev.WebhookTimestamp)) {
		return
	}
	l.tryDispatchIssue(ctx, d, id)
}

// tryDispatchIssue is the idempotent pickup path (shared by an issue event and a pre-dispatch comment):
// fetch the canonical issue, require the trigger label + a to-do state + no running workflow, infer the
// repo, dispatch, and move the issue to In Progress. Every guard makes a duplicate a no-op.
func (l *linearConnector) tryDispatchIssue(ctx context.Context, d *Dispatcher, id string) {
	// Serialize concurrent processing of the SAME issue (e.g. a near-simultaneous label + comment) so two
	// goroutines can't both clear the FindByConversation check before either indexes its workflow.
	unlock := l.issueLock.lock(id)
	defer unlock()

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	iss, err := l.client.Issue(cctx, id)
	if err != nil {
		log.Printf("connectors/linear issue %s: %v", id, err)
		return
	}
	if !iss.HasLabel(linearTriggerLabel) {
		return
	}
	if !isTodoState(iss.State.Type) {
		return // already started / done / canceled — not a fresh pickup
	}
	if wf, err := d.FindByConversation(cctx, "linear:"+id); err == nil && wf != "" {
		return // already being worked
	}
	repo, ok := l.resolveRepo(cctx, iss)
	if !ok {
		l.postClarify(cctx, iss)
		return
	}
	prompt := clampText(strings.TrimSpace(iss.Title+"\n\n"+iss.Description), 8<<10)
	sid, err := d.Dispatch(cctx, control.Conversation{Kind: "linear", Channel: id}, repo, prompt, false, false)
	if err != nil {
		log.Printf("connectors/linear dispatch issue %s: %v", id, err)
		return
	}
	log.Printf("connectors/linear: dispatched %s for issue %s → %s", sid, iss.Identifier, repo)
	// Move to In Progress AFTER a successful start — the state latch that stops re-pickup. Best-effort.
	if err := l.client.MoveState(cctx, id, "in_progress"); err != nil {
		log.Printf("connectors/linear move-in-progress %s: %v", id, err)
	}
}

func (l *linearConnector) handleComment(ctx context.Context, d *Dispatcher, ev linear.Event) {
	if l.viewerID == "" {
		return // fail-closed: we can't tell our own comments apart, so we can't steer safely
	}
	cm := ev.Comment()
	if cm.UserID == l.viewerID {
		return // our own comment — ignore (no echo loop)
	}
	if cm.ID == "" || cm.IssueID == "" || cm.UserID == "" {
		// fail-closed on an author-less/unattributable comment — but log it, since a parse miss here would
		// silently swallow steering.
		log.Printf("connectors/linear: dropping comment (missing id/issue/author): id=%q issue=%q author=%q", cm.ID, cm.IssueID, cm.UserID)
		return
	}
	if l.dedupe.seen("comment:" + cm.ID) {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if wf, err := d.FindByConversation(cctx, "linear:"+cm.IssueID); err == nil && wf != "" {
		if err := d.Signal(cctx, wf, control.SignalUserMessage, control.UserMessageSignal{Text: cm.Body}); err != nil {
			log.Printf("connectors/linear signal %s: %v", wf, err)
			return
		}
		log.Printf("connectors/linear: user_message → %s (issue %s)", wf, cm.IssueID)
		return
	}
	// No running workflow — the comment may be a repo clarification, so re-attempt the pickup path
	// (idempotent; a resolved repo now dispatches, an unresolved one re-asks once).
	l.tryDispatchIssue(ctx, d, cm.IssueID)
}

// resolveRepo infers the repo for an issue. With an allowlist set it is a CONSTRAINT: a single entry
// short-circuits, otherwise a cheap LLM picks one slug from the list and the answer is VALIDATED against
// it. With NO allowlist the GitHub App's installed repos are the boundary, so the LLM infers the repo
// free-form and we attempt it — an unreachable repo fails visibly on the issue. Uncertainty → the
// default if set, else unresolved (caller asks). Never dispatches to a guessed repo without either the
// allowlist validation or a well-formed inferred slug.
func (l *linearConnector) resolveRepo(ctx context.Context, iss *linear.Issue) (string, bool) {
	switch len(l.allowlist) {
	case 0:
		if l.defaultRepo != "" {
			return l.defaultRepo, true
		}
		return l.inferRepoFreeform(ctx, iss)
	case 1:
		return l.allowlist[0], true
	}
	var sys strings.Builder
	sys.WriteString("Pick the ONE repository slug from this list that the issue below is about, or reply ")
	sys.WriteString("UNRESOLVED if you cannot tell. Reply with ONLY the slug or UNRESOLVED. Repositories:\n")
	for _, r := range l.allowlist {
		sys.WriteString("- " + r + "\n")
	}
	user := iss.Title + "\n\n" + iss.Description
	for _, c := range iss.Comments {
		if c.UserID != l.viewerID { // human context can disambiguate
			user += "\n" + c.Body
		}
	}
	// Strip formatting the model may add (backticks/quotes/trailing punctuation) so a correct-but-decorated
	// answer still matches an allowlist entry rather than silently falling through to the default.
	ans := strings.Trim(strings.TrimSpace(l.llm.Classify(ctx, sys.String(), clampText(user, 6<<10))), "`'\".,;:")
	for _, r := range l.allowlist {
		if strings.EqualFold(ans, r) { // Classify lowercases; return the canonical allowlist form
			return r, true
		}
	}
	if l.defaultRepo != "" {
		return l.defaultRepo, true
	}
	return "", false
}

// inferRepoFreeform asks the LLM to name the repo the issue is about, as owner/repo, with NO allowlist to
// pick from. The issue is expected to name the repo explicitly; the answer is shape-validated but not checked
// against a list — the GitHub App's installed repos are the boundary, and a repo it can't reach fails visibly
// on the issue. Anything that isn't a well-formed owner/repo → ("", false) (caller asks).
func (l *linearConnector) inferRepoFreeform(ctx context.Context, iss *linear.Issue) (string, bool) {
	const sys = "Name the GitHub repository this issue is about, as owner/repo. " +
		"Reply with ONLY the slug (owner/repo) or UNRESOLVED if you cannot tell."
	user := iss.Title + "\n\n" + iss.Description
	for _, c := range iss.Comments {
		if c.UserID != l.viewerID { // human context can disambiguate
			user += "\n" + c.Body
		}
	}
	return normalizeRepoAnswer(l.llm.Classify(ctx, sys, clampText(user, 6<<10)))
}

// normalizeRepoAnswer turns a raw LLM repo answer into a validated owner/repo slug: strip decoration, treat
// UNRESOLVED/empty as a miss, and require a well-formed owner/repo. Returns ("", false) on anything it can't
// validate (including a bare repo name — issues must name the owner) — never a guessed shape.
func normalizeRepoAnswer(ans string) (string, bool) {
	ans = strings.Trim(strings.TrimSpace(ans), "`'\".,;:")
	if ans == "" || strings.EqualFold(ans, "unresolved") || !validRepoSlug(ans) {
		return "", false
	}
	return ans, true
}

// validRepoSlug reports whether s is exactly owner/repo, both segments non-empty and composed of
// GitHub-legal characters (letters, digits, '.', '_', '-').
func validRepoSlug(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return false
	}
	return isRepoToken(owner) && isRepoToken(repo)
}

func isRepoToken(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// postClarify asks (once per human turn) which repo the issue is about — from the allowlist if one is
// configured, otherwise as a free-form owner/repo.
func (l *linearConnector) postClarify(ctx context.Context, iss *linear.Issue) {
	if l.alreadyAsked(iss) {
		return
	}
	body := "I couldn't tell which repository this issue is about. "
	if len(l.allowlist) > 0 {
		body += "Reply with one of:\n"
		for _, r := range l.allowlist {
			body += "- `" + r + "`\n"
		}
	} else {
		body += "Reply with the repository as `owner/repo`."
	}
	if _, err := l.client.CreateComment(ctx, iss.ID, body); err != nil {
		log.Printf("connectors/linear clarify %s: %v", iss.ID, err)
	}
}

// alreadyAsked reports whether our most recent comment is a clarify prompt still awaiting a human reply
// (heuristic: the newest comment is ours), so we don't re-ask on every webhook. Before dispatch our only
// comments are clarify prompts, so this is safe.
func (l *linearConnector) alreadyAsked(iss *linear.Issue) bool {
	if l.viewerID == "" || len(iss.Comments) == 0 {
		return false
	}
	return iss.Comments[len(iss.Comments)-1].UserID == l.viewerID
}

// ---- pure helpers ----

func isTodoState(stateType string) bool {
	switch stateType {
	case "triage", "backlog", "unstarted":
		return true
	}
	return false
}

// splitRepos parses a space/comma/newline-separated repo list.
func splitRepos(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' || r == '\t' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func clampText(s string, max int) string {
	if len(s) > max {
		return s[:max]
	}
	return s
}

// lruSet is a bounded, FIFO-trimmed dedup set (mirrors the workflow's delivery dedupe).
type lruSet struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
	cap   int
}

func newLRUSet(capacity int) *lruSet {
	return &lruSet{set: map[string]struct{}{}, cap: capacity}
}

// seen records key and returns whether it was ALREADY present (i.e. a duplicate).
func (l *lruSet) seen(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.set[key]; ok {
		return true
	}
	l.set[key] = struct{}{}
	l.order = append(l.order, key)
	if len(l.order) > l.cap {
		delete(l.set, l.order[0])
		l.order = l.order[1:]
	}
	return false
}

// keyedMutex hands out a lock per key, so callers can serialize work on the same key while different keys
// proceed concurrently. Entries are retained (one *sync.Mutex per distinct key ever seen) — bounded by the
// number of distinct issues, negligible on a single box.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{m: map[string]*sync.Mutex{}} }

// lock acquires the mutex for key and returns its releaser.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	mu, ok := k.m[key]
	if !ok {
		mu = &sync.Mutex{}
		k.m[key] = mu
	}
	k.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}
