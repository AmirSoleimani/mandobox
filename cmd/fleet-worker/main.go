// Command fleet-worker hosts the Temporal PRWorkflow and its activities (PLAN §6). It runs on
// the fleet host: it dials Temporal (localhost), reaches fleet-agent over mTLS, mints GitHub
// App tokens, and talks to guests over NATS. Tier-0 secrets (the App private key) stay here.
package main

import (
	"log"
	"os"

	"github.com/acme/fleet/internal/control"
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
		slackToken, slackChannel         string
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
		botUser:      env("GITHUB_BOT_USER", "fleet-agent[bot]"),
		botEmail:     env("GITHUB_BOT_EMAIL", "fleet-agent[bot]@users.noreply.github.com"),
		slackToken:   os.Getenv("SLACK_BOT_TOKEN"),
		slackChannel: os.Getenv("SLACK_CHANNEL"),
	}
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
		Fleet:         fleet,
		App:           app,
		NATSURL:       cfg.natsURL,
		GatewayURL:    cfg.gatewayURL,
		BotUser:       cfg.botUser,
		BotEmail:      cfg.botEmail,
		SlackBotToken: cfg.slackToken,
		SlackChannel:  cfg.slackChannel,
	}

	w := worker.New(c, control.TaskQueue, worker.Options{})
	w.RegisterWorkflow(control.PRWorkflow)
	w.RegisterActivity(acts)

	log.Printf("fleet-worker: task-queue=%s namespace=%s temporal=%s", control.TaskQueue, cfg.namespace, cfg.temporalAddr)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("worker: %v", err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
