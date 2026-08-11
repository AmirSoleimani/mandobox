// Command mando-worker hosts the Temporal PRWorkflow and its activities. It runs on
// the fleet host: it dials Temporal (localhost), reaches mando-agent over mTLS, mints GitHub
// App tokens, and talks to guests over NATS. Tier-0 secrets (the App private key) stay here.
package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/acme/mandobox/internal/connectors"
	"github.com/acme/mandobox/internal/control"
	"github.com/acme/mandobox/internal/reconcile"
	"github.com/acme/mandobox/internal/supervisor"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func main() {
	cfg := struct {
		temporalAddr, namespace          string
		fleetURL, tlsCert, tlsKey, serCA string
		natsURL, gatewayURL              string
		appID, appKeyPath, org           string
		instID, botUser, botEmail        string
	}{
		temporalAddr: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		namespace:    env("TEMPORAL_NAMESPACE", "fleet"),
		fleetURL:     env("FLEET_URL", "https://127.0.0.1:9443"),
		tlsCert:      env("FLEET_TLS_CERT", "/etc/fleet/tls/reconciler.crt"),
		tlsKey:       env("FLEET_TLS_KEY", "/etc/fleet/tls/reconciler.key"),
		serCA:        env("FLEET_SERVER_CA", "/etc/fleet/tls/server-ca.crt"),
		natsURL:      env("NATS_URL", "nats://172.31.0.1:4222"),
		gatewayURL:   env("GATEWAY_URL", "http://172.31.0.1:8080"),
		appID:        os.Getenv("GITHUB_APP_ID"),
		appKeyPath:   os.Getenv("GITHUB_APP_KEY"),
		org:          os.Getenv("GITHUB_ORG"),
		instID:       os.Getenv("GITHUB_INSTALLATION_ID"),
		botUser:      env("GITHUB_BOT_USER", "mando-agent[bot]"),
		botEmail:     env("GITHUB_BOT_EMAIL", "mando-agent[bot]@users.noreply.github.com"),
	}
	// Optional pre-authenticated `code tunnel` token — injected into every guest so a human attach
	// skips the device login. Absent → operators device-login on first attach.
	vscodeTunnelToken := ""
	if b, err := os.ReadFile(env("VSCODE_TUNNEL_TOKEN_FILE", "/etc/fleet/vscode-tunnel-token.json")); err == nil {
		vscodeTunnelToken = string(b)
	}
	// The VS Code CLI binds the token to the hostname it was minted under. Operators run
	// `code tunnel user login` on this host, so the guest must adopt this host's name for the
	// injected token to validate. Overridable if the token was minted elsewhere.
	vscodeTunnelHostname := os.Getenv("VSCODE_TUNNEL_HOSTNAME")
	if vscodeTunnelHostname == "" {
		vscodeTunnelHostname, _ = os.Hostname()
	}
	// Operator config path (defaults + guardrails). Re-read per dispatch so edits — e.g. toggling an
	// agent in agents_allowed — take effect on the next dispatch with no restart.
	boxConfigPath := env("MANDOBOX_CONFIG", "/etc/fleet/mandobox.yml")
	// Box-wide default agent instructions (dashboard-managed plain-text file). Re-read per dispatch.
	instructionsPath := env("MANDO_INSTRUCTIONS", "/etc/fleet/agent-instructions.md")
	// Operator preamble overrides (dashboard-managed). Materialize the built-in defaults to <path>.default
	// so the dashboard can show them and offer reset; the override files themselves start absent.
	preambleAutoPath := env("MANDO_PREAMBLE_AUTONOMOUS", "/etc/fleet/preamble-autonomous.md")
	preambleCollabPath := env("MANDO_PREAMBLE_COLLABORATE", "/etc/fleet/preamble-collaborate.md")
	// Agent auth mode (dashboard toggle) + the Claude subscription token — docs/subscription-auth.md.
	authModePath := env("MANDO_AGENT_AUTH", "/etc/fleet/agent-auth")
	oauthTokenPath := env("MANDO_CLAUDE_OAUTH_TOKEN", "/etc/fleet/claude-oauth-token")
	providerConfigPath := env("MANDO_PROVIDER_CONFIG", "/etc/fleet/provider.json")
	metaDir := env("FLEET_LOG_DIR", "/var/lib/fleet/logs")
	// NATS decentralized-auth material (closes the unauthenticated-bus finding). Absent files → the
	// worker mints no per-session creds and connects unauthenticated (legacy bus) — safe until the
	// server cutover provisions these and requires auth.
	natsServiceCreds := env("MANDO_NATS_SERVICE_CREDS", "/etc/fleet/nats-service.creds")
	natsAccountSeed := readTrimFile(env("MANDO_NATS_ACCOUNT_SEED", "/etc/fleet/nats-account.seed"))
	natsAccountPub := readTrimFile(env("MANDO_NATS_ACCOUNT_PUBKEY", "/etc/fleet/nats-account.pub"))
	writePreambleDefault(preambleAutoPath+".default", supervisor.DefaultAutonomousPreamble)
	writePreambleDefault(preambleCollabPath+".default", supervisor.DefaultCollaboratePreamble)
	if cfg.appID == "" || cfg.appKeyPath == "" {
		log.Fatal("GITHUB_APP_ID and GITHUB_APP_KEY are required")
	}

	keyPEM, err := os.ReadFile(cfg.appKeyPath)
	if err != nil {
		log.Fatalf("read app key: %v", err)
	}
	app, err := control.NewGitHubApp(cfg.appID, cfg.org, cfg.instID, keyPEM)
	if err != nil {
		log.Fatalf("github app: %v", err)
	}
	fleet, err := control.NewFleetClient(cfg.fleetURL, cfg.tlsCert, cfg.tlsKey, cfg.serCA)
	if err != nil {
		log.Fatalf("fleet client: %v", err)
	}

	c, err := client.Dial(client.Options{HostPort: cfg.temporalAddr, Namespace: cfg.namespace})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer c.Close()

	acts := &control.Activities{
		Fleet:                   fleet,
		App:                     app,
		NATSURL:                 cfg.natsURL,
		GatewayURL:              cfg.gatewayURL,
		BotUser:                 cfg.botUser,
		BotEmail:                cfg.botEmail,
		VSCodeTunnelToken:       vscodeTunnelToken,
		VSCodeTunnelHostname:    vscodeTunnelHostname,
		BoxConfigPath:           boxConfigPath,
		InstructionsPath:        instructionsPath,
		PreambleAutonomousPath:  preambleAutoPath,
		PreambleCollaboratePath: preambleCollabPath,
		AuthModePath:            authModePath,
		OAuthTokenPath:          oauthTokenPath,
		ProviderConfigPath:      providerConfigPath,
		MetaDir:                 metaDir,
		NATSAccountSeed:         natsAccountSeed,
		NATSAccountPubKey:       natsAccountPub,
		NATSServiceCreds:        natsServiceCreds,
		// Orphan-VM reaper (the scheduled ReconcileWorkflow, replacing the standalone reconciler):
		// a VM is expected iff its session's workflow is Running.
		ReconcileAuthority: reconcile.NewTemporalAuthority(c, cfg.namespace),
		ReconcileGrace:     parseDurationOr(env("RECONCILE_GRACE", "3m"), 3*time.Minute),
	}

	// Register the outbound half of every enabled + configured chat connector (Slack, Telegram, …). The
	// set is governed by connectors.json + the per-connector secret env; the dashboard toggles it and
	// restarts this worker. No lazy fallback — a disabled connector simply isn't registered.
	connCfg := connectors.LoadConfig(env("CONNECTORS_CONFIG", "/etc/fleet/connectors.json"))
	acts.Notifiers = map[string]control.Notifier{}
	for _, c := range connectors.Registry() {
		if connectors.Enabled(connCfg, c) {
			if n := c.Notifier(); n != nil {
				acts.Notifiers[c.Kind()] = n
				log.Printf("mando-worker: connector %q outbound registered", c.Kind())
			}
		}
	}

	w := worker.New(c, control.TaskQueue, worker.Options{})
	w.RegisterWorkflow(control.PRWorkflow)
	w.RegisterWorkflow(control.ReconcileWorkflow)
	w.RegisterActivity(acts)

	// Create the reconcile schedule if absent (idempotent across restarts). Non-fatal: the worker
	// still serves PRWorkflows even if scheduling hiccups.
	if err := control.EnsureReconcileSchedule(context.Background(), c, control.ReconcileInterval); err != nil {
		log.Printf("mando-worker: reconcile schedule not ensured (continuing): %v", err)
	}

	log.Printf("mando-worker: task-queue=%s namespace=%s temporal=%s", control.TaskQueue, cfg.namespace, cfg.temporalAddr)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

// readTrimFile returns the trimmed contents of path, or "" if it's absent/unreadable — so a secret
// not yet provisioned degrades to the legacy (unauthenticated) path rather than crashing the worker.
func readTrimFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// parseDurationOr parses a Go duration string, falling back to def on any error.
func parseDurationOr(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

// writePreambleDefault materializes a built-in preamble to a .default file the dashboard reads as the
// editable baseline / reset target. Best-effort: a failure just means the dashboard shows no default.
func writePreambleDefault(path, content string) {
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("mando-worker: could not write preamble default %s: %v", path, err)
	}
}
