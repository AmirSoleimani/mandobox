// Command mando-dashboard is the single-box management console for a mandobox fleet. It runs on the
// fleet host and is meant to be reached over an SSH tunnel (it binds to localhost by default), so it
// carries no auth of its own — access is gated by who can reach the box.
//
// It observes and manages:
//   - Sessions:     the live/recent PRWorkflow executions, read from Temporal (visibility + "status").
//   - VMs:          the live microVMs from fleet-agent's mTLS /vms, orphan-flagged against Temporal.
//   - Config:       /etc/fleet/mandobox.yml, edited as raw YAML with a key reference (re-read per dispatch).
//   - Instructions: the box-wide default agent instructions file (re-read per dispatch).
//   - Tools:        pinned agent-CLI versions + the active image, with in-place update-tools.sh runs.
//   - Secrets:      status + rotation of the box's secrets (fingerprints only, never values).
//
// Everything is served from a single self-contained binary (frontend embedded via go:embed).
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	temporal     *temporalStore
	config       *configStore
	tools        *toolStore
	vms          *vmStore
	secrets      *secretStore
	instructions *instructionsStore
	preambles    *preambleStore
	activity     *activityStore
	dispatch     *dispatchStore
	health       *healthStore
	costs        *costStore
	provider     *providerStore
	connectors   *connectorStore
	meta         *metaStore
}

func main() {
	addr := flag.String("addr", env("MANDO_DASHBOARD_ADDR", "127.0.0.1:8087"), "listen address (localhost by default; reach it over an SSH tunnel)")
	temporalAddr := flag.String("temporal", env("TEMPORAL_ADDRESS", "127.0.0.1:7233"), "Temporal frontend host:port")
	namespace := flag.String("namespace", env("TEMPORAL_NAMESPACE", "fleet"), "Temporal namespace")
	configPath := flag.String("config", env("MANDOBOX_CONFIG", "/etc/fleet/mandobox.yml"), "path to the box config")
	toolsEnv := flag.String("tools-env", env("MANDO_TOOLS_ENV", "/usr/local/lib/fleet/tools.env"), "pinned agent-CLI versions manifest")
	updateTools := flag.String("update-tools", env("MANDO_UPDATE_TOOLS", "/usr/local/lib/fleet/update-tools.sh"), "the update-tools.sh entry point")
	auditLog := flag.String("audit", env("MANDO_TOOL_AUDIT", "/var/lib/fleet/tool-updates.log"), "tool-update audit log")
	imagesDir := flag.String("images-dir", env("MANDO_IMAGES_DIR", "/var/lib/fleet/images"), "golden-image directory (holds current.sha)")
	fleetURL := flag.String("fleet-url", env("FLEET_URL", "https://127.0.0.1:9443"), "mando-agent mTLS API base (for VM reports)")
	tlsCert := flag.String("tls-cert", env("FLEET_TLS_CERT", "/etc/fleet/tls/reconciler.crt"), "client cert for the mando-agent API")
	tlsKey := flag.String("tls-key", env("FLEET_TLS_KEY", "/etc/fleet/tls/reconciler.key"), "client key for the mando-agent API")
	tlsCA := flag.String("tls-ca", env("FLEET_SERVER_CA", "/etc/fleet/tls/server-ca.crt"), "server CA for the mando-agent API")
	secretAudit := flag.String("secret-audit", env("MANDO_SECRET_AUDIT", "/var/lib/fleet/secret-rotations.log"), "secret-rotation audit log")
	instructionsPath := flag.String("instructions", env("MANDO_INSTRUCTIONS", "/etc/fleet/agent-instructions.md"), "box-wide default agent instructions file")
	preambleAuto := flag.String("preamble-autonomous", env("MANDO_PREAMBLE_AUTONOMOUS", "/etc/fleet/preamble-autonomous.md"), "autonomous-turn preamble override file")
	preambleCollab := flag.String("preamble-collaborate", env("MANDO_PREAMBLE_COLLABORATE", "/etc/fleet/preamble-collaborate.md"), "collaborate-turn preamble override file")
	logDir := flag.String("log-dir", env("FLEET_LOG_DIR", "/var/lib/fleet/logs"), "archived guest agent-activity logs (<session>.log.jsonl)")
	dispatchBin := flag.String("dispatch-bin", env("MANDO_DISPATCH", "/usr/local/bin/mando-dispatch"), "the mando-dispatch entry point (New Session)")
	litellmAddr := flag.String("litellm", env("LITELLM_ADDR", "127.0.0.1:4000"), "LiteLLM host:port (health check)")
	natsAddr := flag.String("nats", env("NATS_ADDR", "172.31.0.1:4222"), "NATS host:port (health check)")
	diskPath := flag.String("disk-path", env("FLEET_DATA_DIR", "/var/lib/fleet"), "fleet data dir (disk health check)")
	providerPath := flag.String("provider-config", env("MANDO_PROVIDER_CONFIG", "/etc/fleet/provider.json"), "active-provider selection file (Config → Model)")
	connectorsConfig := flag.String("connectors-config", env("MANDO_CONNECTORS_CONFIG", "/etc/fleet/connectors.json"), "connector enable/disable config (connectors.json)")
	flag.Parse()

	temporal := newTemporalStore(*temporalAddr, *namespace)
	vms := newVMStore(*fleetURL, *tlsCert, *tlsKey, *tlsCA)
	tools := newToolStore(*toolsEnv, *updateTools, *auditLog, *imagesDir)
	secrets := newSecretStore(*secretAudit)

	s := &server{
		temporal:     temporal,
		config:       newConfigStore(*configPath),
		tools:        tools,
		vms:          vms,
		secrets:      secrets,
		instructions: newInstructionsStore(*instructionsPath),
		preambles:    newPreambleStore(*preambleAuto, *preambleCollab),
		activity:     newActivityStore(*logDir),
		dispatch:     newDispatchStore(*dispatchBin, *imagesDir, *temporalAddr, *namespace),
		health:       newHealthStore(*temporalAddr, *litellmAddr, *natsAddr, *fleetURL, *diskPath, vms, tools, secrets),
		costs:        newCostStore(*logDir),
		provider:     newProviderStore(*providerPath, secrets),
		connectors:   newConnectorStore(secrets, *connectorsConfig),
		meta:         newMetaStore(*logDir),
	}
	defer s.temporal.close()

	web, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed web: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/sessions/{id}/activity", s.handleActivity)
	mux.HandleFunc("/api/sessions/{id}/message", s.handleSessionMessage)
	mux.HandleFunc("/api/sessions/{id}/abort", s.handleAbort)
	mux.HandleFunc("/api/sessions/{id}/attach", s.handleAttach)
	mux.HandleFunc("/api/sessions/{id}/terminate", s.handleTerminate)
	mux.HandleFunc("/api/dispatch", s.handleDispatch)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/costs", s.handleCosts)
	mux.HandleFunc("/api/connectors", s.handleConnectors)
	mux.HandleFunc("/api/connectors/enable", s.handleConnectorEnable)
	mux.HandleFunc("/api/provider", s.handleProvider)
	mux.HandleFunc("/api/provider/activate", s.handleProviderActivate)
	mux.HandleFunc("/api/provider/secret", s.handleProviderSecret)
	mux.HandleFunc("/api/provider/model", s.handleProviderModel)
	mux.HandleFunc("/api/vms", s.handleVMs)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/schema", s.handleConfigSchema)
	mux.HandleFunc("/api/instructions", s.handleInstructions)
	mux.HandleFunc("/api/preambles", s.handlePreambles)
	mux.HandleFunc("/api/tools", s.handleTools)
	mux.HandleFunc("/api/tools/update", s.handleToolsUpdate)
	mux.HandleFunc("/api/secrets", s.handleSecrets)
	mux.HandleFunc("/api/secrets/rotate", s.handleSecretRotate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.Handle("/", http.FileServer(http.FS(web)))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           sameOrigin(logRequests(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("mando-dashboard: listening on http://%s (temporal=%s ns=%s config=%s)", *addr, *temporalAddr, *namespace, *configPath)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("mando-dashboard: stopped")
}

// ---- handlers ------------------------------------------------------------

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	sessions, err := s.temporal.sessions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	s.enrichSessionsDurable(sessions)
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

// enrichSessionsDurable fills fields the live "status" query can't provide for CLOSED sessions, from
// sources that outlive the workflow: archived cost from the .event.jsonl logs, and the PR URL rebuilt
// from the pr_number search attribute. So a merged/closed session keeps its cost and its PR link.
func (s *server) enrichSessionsDurable(sessions []Session) {
	cost := map[string]float64{}
	for _, sc := range s.costs.perSession() {
		cost[sc.SessionID] = sc.CostUSD
	}
	meta := s.meta.all()
	for i := range sessions {
		ss := &sessions[i]
		if ss.CostUSD == 0 {
			if c, ok := cost[ss.WorkflowID]; ok {
				ss.CostUSD = c
			}
		}
		if ss.PRNumber > 0 && ss.PRURL == "" && ss.Repo != "" {
			ss.PRURL = "https://github.com/" + ss.Repo + "/pull/" + strconv.Itoa(ss.PRNumber)
		}
		if m, ok := meta[ss.WorkflowID]; ok {
			ss.Model = m.Model
			ss.Provider = m.Provider
			ss.Subscription = m.Subscription
		}
	}
}

// handleActivity streams a session's agent activity as Server-Sent Events (scrollback + live tail).
func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	path, ok := s.activity.logPath(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusBadRequest, errMsg("invalid session id"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errMsg("streaming unsupported"))
		return
	}
	sseHeaders(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	s.activity.stream(r.Context(), w, flusher.Flush, path)
}

// handleSessionMessage prompts a session's agent — a "user_message" signal to its workflow (the same
// durable path Slack uses). Works while Running or awaiting_review; a closed workflow returns an error.
func (s *server) handleSessionMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	id := r.PathValue("id")
	if !sessionIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, errMsg("invalid session id"))
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeErr(w, http.StatusBadRequest, errMsg("empty message"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.temporal.sendUserMessage(ctx, id, body.Text); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// sessionAction is the shared shape for abort/terminate (both take an optional reason).
func (s *server) sessionAction(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, id, reason string) error) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	id := r.PathValue("id")
	if !sessionIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, errMsg("invalid session id"))
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if strings.TrimSpace(body.Reason) == "" {
		body.Reason = "operator action from dashboard"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := fn(ctx, id, body.Reason); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleAbort(w http.ResponseWriter, r *http.Request) {
	s.sessionAction(w, r, s.temporal.abort)
}

func (s *server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	s.sessionAction(w, r, s.temporal.terminate)
}

func (s *server) handleAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	id := r.PathValue("id")
	if !sessionIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, errMsg("invalid session id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.temporal.attach(ctx, id); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var req dispatchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sid, err := s.dispatch.dispatch(ctx, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid})
}

// handleConnectors returns the integrations (GitHub, Slack, …) with each one's secret status.
// Setting a connector's secret reuses the existing /api/secrets/rotate endpoint.
func (s *server) handleConnectors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"connectors": s.connectors.view()})
}

// handleConnectorEnable toggles a connector on/off: it writes connectors.json and restarts the worker
// (outbound) and the connector host (inbound), which read that config at startup.
func (s *server) handleConnectorEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id required"})
		return
	}
	if err := s.connectors.setEnabled(body.ID, body.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var restarted []string
	for _, candidates := range [][]string{{"mando-worker", "fleet-worker"}, {"mando-connectors"}} {
		if u, err := restartFirst(ctx, candidates); err == nil {
			restarted = append(restarted, u)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": s.connectors.view(), "restarted": restarted})
}

// handleProvider returns the provider catalog + which one is active (Config → Model).
func (s *server) handleProvider(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.provider.view())
}

// handleProviderActivate switches the box-wide active provider (guarded on its secret being set).
func (s *server) handleProviderActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := s.provider.activate(body.ID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleProviderModel records a provider's chosen main/helper model.
func (s *server) handleProviderModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var body struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		CheapModel string `json:"cheap_model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	v, err := s.provider.setModel(body.ID, body.Model, body.CheapModel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleProviderSecret writes/rotates a provider's backing secret (API key or OAuth token) inline,
// so the operator sets everything a provider needs from its card — no trip to the Secrets page.
func (s *server) handleProviderSecret(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var body struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	v, restarted, err := s.provider.setSecret(ctx, body.ID, body.Value)
	if err != nil {
		// The value may be written even if a restart failed — surface both, don't hard-fail.
		writeJSON(w, http.StatusOK, map[string]any{"view": v, "restarted": restarted, "warning": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"view": v, "restarted": restarted, "ok": true})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"checks": s.health.report(ctx)})
}

func (s *server) handleCosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	meta, _ := s.temporal.sessionMetadata(ctx) // repo/time attribution; empty on error → "(unknown)"
	writeJSON(w, http.StatusOK, buildCostReport(s.costs.perSession(), meta, s.meta.all()))
}

func (s *server) handleVMs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	vms, err := s.vms.list(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// Orphan detection needs the Running set; if Temporal is unreachable we still show the VMs,
	// just without orphan flags (enrichVMs suppresses them when running is nil).
	running, _ := s.temporal.runningSessionIDs(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"vms": enrichVMs(vms, running, nowFn())})
}

func (s *server) handleConfigSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, configHelpDoc())
}

func (s *server) handleInstructions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		v, err := s.instructions.read()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	case http.MethodPut:
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.instructions.write(body.Raw); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		v, err := s.instructions.read()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
	}
}

func (s *server) handlePreambles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"preambles": s.preambles.view()})
	case http.MethodPut:
		var body struct {
			Name string `json:"name"`
			Raw  string `json:"raw"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.preambles.write(body.Name, body.Raw); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"preambles": s.preambles.view()})
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
	}
}

func (s *server) handleSecrets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"secrets": s.secrets.view()})
}

func (s *server) handleSecretRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var body struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	restarted, err := s.secrets.rotate(ctx, body.Name, body.Value)
	if err != nil {
		// A restart failure still means the value was written — report both.
		writeJSON(w, http.StatusOK, map[string]any{"restarted": restarted, "warning": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restarted": restarted, "ok": true})
}

func (s *server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		v, err := s.config.read()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	case http.MethodPut:
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.config.write(body.Raw); err != nil {
			// A validation failure is the operator's to fix — report it as a 400 with the reason.
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		v, err := s.config.read()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
	}
}

func (s *server) handleTools(w http.ResponseWriter, r *http.Request) {
	v, err := s.tools.view()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) handleToolsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, errMsg("method not allowed"))
		return
	}
	var body struct {
		Claude string `json:"claude"`
		Codex  string `json:"codex"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	job, err := s.tools.startUpdate(body.Claude, body.Codex)
	if err != nil {
		// Already-running is a conflict, not a server fault.
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "job": job})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

// ---- helpers -------------------------------------------------------------

type errMsg string

func (e errMsg) Error() string { return string(e) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// sameOrigin defends the root-privileged, auth-less localhost API from cross-origin/CSRF and
// DNS-rebinding. The dashboard has no auth of its own (access == who can reach localhost over the SSH
// tunnel), so a page in the operator's browser must not be able to drive /api/secrets/rotate,
// /api/tools/update, /api/dispatch, etc. GET/HEAD/OPTIONS pass through (SSE + reads). State-changing
// methods require: (a) a local Host (blunts DNS-rebinding), (b) Sec-Fetch-Site same-origin/none when
// the browser sends it, and (c) Content-Type application/json — a non-CORS-simple type, so any
// cross-origin attempt is forced into a preflight that fails closed (the server emits no CORS headers).
func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := hostname(r.Host); h != "" && !isLocalHost(h) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// safe/idempotent (reads + SSE) — let through
		default:
			if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			ct := r.Header.Get("Content-Type")
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = ct[:i]
			}
			if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostname strips any :port from a Host header.
func hostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// isLocalHost reports whether h is a loopback/private address or "localhost" — anything else (a public
// hostname resolving to loopback) is a DNS-rebinding attempt and is refused.
func isLocalHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
