// Command fleet-reconciler is the control-plane safety net that kills orphaned microVMs:
// VMs the fleet host is running that the authority no longer claims (PLAN §7.7). In M2 the
// authority is a JSON file; M4 replaces it with one backed by Temporal's open workflows.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/acme/fleet/internal/reconcile"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	base := flag.String("fleet-url", envOr("FLEET_URL", "https://127.0.0.1:9443"), "fleet-agent base URL")
	tlsCert := flag.String("tls-cert", envOr("FLEET_TLS_CERT", "/etc/fleet/tls/reconciler.crt"), "client certificate")
	tlsKey := flag.String("tls-key", envOr("FLEET_TLS_KEY", "/etc/fleet/tls/reconciler.key"), "client private key")
	serverCA := flag.String("server-ca", envOr("FLEET_SERVER_CA", "/etc/fleet/tls/server-ca.crt"), "CA that signs fleet-agent's cert")
	authFile := flag.String("authority-file", envOr("FLEET_AUTHORITY_FILE", "/etc/fleet/expected-sessions.json"), "expected-sessions file")
	interval := flag.Duration("interval", 5*time.Minute, "reconcile interval")
	grace := flag.Duration("grace", 3*time.Minute, "exempt VMs younger than this")
	flag.Parse()

	tlsCfg, err := reconcile.LoadClientTLS(*tlsCert, *tlsKey, *serverCA)
	if err != nil {
		log.Error("load TLS", "err", err)
		os.Exit(1)
	}

	client := reconcile.NewHTTPClient(*base, tlsCfg)
	authority := reconcile.NewFileAuthority(*authFile)
	r := reconcile.New(client, authority, reconcile.Options{Grace: *grace, Log: log})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("fleet-reconciler starting", "fleet_url", *base, "interval", interval.String(), "grace", grace.String())
	r.Run(ctx, *interval)
	log.Info("fleet-reconciler stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
