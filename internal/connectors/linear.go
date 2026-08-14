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
	allowlist     []string // repos the agent may work on; grounds + validates the LLM repo inference
	defaultRepo   string   // fallback when the repo can't be inferred (optional)
	model         string   // cheap model for repo inference
	gatewayURL    string

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
		model:         envOr("LINEAR_REPO_MODEL", os.Getenv("CLAUDE_CHEAP_MODEL")),
		gatewayURL:    os.Getenv("GATEWAY_URL"),
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
	l.llm = llm.New(l.gatewayURL, "classify", l.model) // gateway injects the real key; bearer is a placeholder
	l.llm.MaxTokens = 24                               // enough for an owner/name slug or UNRESOLVED

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
	if cm.ID == "" || cm.IssueID == "" || cm.UserID == "" || cm.UserID == l.viewerID {
		return // ignore our own comments; and fail-closed on an author-less comment (can't attribute → can't steer)
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

// resolveRepo infers the repo for an issue: a single-repo allowlist (or a bare default) short-circuits;
// otherwise a cheap LLM picks one slug from the allowlist and the answer is VALIDATED against it. Any
// uncertainty → the default if set, else unresolved (caller asks). Never dispatches to a guessed repo.
func (l *linearConnector) resolveRepo(ctx context.Context, iss *linear.Issue) (string, bool) {
	switch len(l.allowlist) {
	case 0:
		return l.defaultRepo, l.defaultRepo != ""
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

// postClarify asks (once per human turn) which repo the issue is about.
func (l *linearConnector) postClarify(ctx context.Context, iss *linear.Issue) {
	if l.alreadyAsked(iss) {
		return
	}
	body := "I couldn't tell which repository this issue is about. Reply with one of:\n"
	for _, r := range l.allowlist {
		body += "- `" + r + "`\n"
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
