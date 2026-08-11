// Command mando-connectors runs the inbound half of every ENABLED chat connector (Slack, Telegram, …)
// in one process — replacing the per-connector gateway binaries. The enabled set comes from
// connectors.json (dashboard-managed) plus each connector's credential env; the dashboard toggles a
// connector and restarts this host. Outbound (posting) is the worker's job — it registers the same
// connectors' Notifiers from the same config.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/AmirSoleimani/mandobox/internal/connectors"
	"go.temporal.io/sdk/client"
)

func main() {
	namespace := env("TEMPORAL_NAMESPACE", "fleet")
	tc, err := client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"), Namespace: namespace})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer tc.Close()

	d := connectors.NewDispatcher(tc, namespace)
	cfg := connectors.LoadConfig(env("CONNECTORS_CONFIG", "/etc/fleet/connectors.json"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	running := 0
	for _, c := range connectors.Registry() {
		if !connectors.Enabled(cfg, c) {
			log.Printf("mando-connectors: %s disabled or unconfigured — skipping", c.Kind())
			continue
		}
		running++
		wg.Add(1)
		go func(c connectors.Connector) {
			defer wg.Done()
			log.Printf("mando-connectors: %s inbound running", c.Kind())
			if err := c.Serve(ctx, d); err != nil && ctx.Err() == nil {
				log.Printf("mando-connectors: %s Serve exited: %v", c.Kind(), err)
			}
		}(c)
	}
	log.Printf("mando-connectors: %d connector(s) running; waiting for shutdown signal", running)
	<-ctx.Done()
	log.Printf("mando-connectors: shutting down")
	wg.Wait()
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
