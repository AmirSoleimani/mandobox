// Command mando-agent is the fleet host's microVM API. It is mTLS-only; the control plane
// initiates every call. It creates and destroys Firecracker microVMs and never
// holds policy or long-lived credentials.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chelodo/mandobox/internal/fleetagent"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg := fleetagent.DefaultConfig()
	listen := flag.String("listen", envOr("FLEET_LISTEN", ":9443"), "mTLS listen address")
	tlsCert := flag.String("tls-cert", envOr("FLEET_TLS_CERT", "/etc/fleet/tls/server.crt"), "server certificate")
	tlsKey := flag.String("tls-key", envOr("FLEET_TLS_KEY", "/etc/fleet/tls/server.key"), "server private key")
	clientCA := flag.String("client-ca", envOr("FLEET_CLIENT_CA", "/etc/fleet/tls/client-ca.crt"), "CA that signs control-plane client certs")
	flag.StringVar(&cfg.ImagesDir, "images-dir", cfg.ImagesDir, "golden rootfs cache")
	flag.StringVar(&cfg.WorkspacesDir, "workspaces-dir", cfg.WorkspacesDir, "persistent workspace volumes")
	flag.StringVar(&cfg.KernelsDir, "kernels-dir", cfg.KernelsDir, "guest kernels")
	flag.StringVar(&cfg.KernelPath, "kernel", cfg.KernelPath, "default guest kernel")
	flag.StringVar(&cfg.JailDir, "jail-dir", cfg.JailDir, "jailer chroot base")
	flag.StringVar(&cfg.RunStateDir, "run-dir", cfg.RunStateDir, "per-VM runtime state (tmpfs)")
	flag.StringVar(&cfg.FirecrackerBin, "firecracker-bin", cfg.FirecrackerBin, "firecracker binary")
	flag.StringVar(&cfg.JailerBin, "jailer-bin", cfg.JailerBin, "jailer binary")
	flag.StringVar(&cfg.GuestSubnet, "guest-subnet", cfg.GuestSubnet, "supernet for per-VM /30s")
	flag.StringVar(&cfg.HostGatewayIP, "host-gw", cfg.HostGatewayIP, "host service anchor address")
	flag.IntVar(&cfg.JailUID, "jail-uid", cfg.JailUID, "uid Firecracker is dropped to")
	flag.IntVar(&cfg.JailGID, "jail-gid", cfg.JailGID, "gid Firecracker is dropped to")
	flag.IntVar(&cfg.MaxVMs, "max-vms", cfg.MaxVMs, "capacity ceiling")
	flag.IntVar(&cfg.WorkspaceSizeMiB, "workspace-size-mib", cfg.WorkspaceSizeMiB, "new workspace size")
	flag.Parse()

	tlsCfg, err := fleetagent.LoadServerTLS(*tlsCert, *tlsKey, *clientCA)
	if err != nil {
		log.Error("load TLS", "err", err)
		os.Exit(1)
	}

	runner := fleetagent.NewExecRunner()
	driver := fleetagent.NewFirecrackerDriver(cfg, runner)
	mgr := fleetagent.NewManager(cfg, runner, driver, log)
	srv := fleetagent.NewServer(mgr, log)

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("mando-agent listening", "addr", *listen, "max_vms", cfg.MaxVMs)
		if err := httpSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
