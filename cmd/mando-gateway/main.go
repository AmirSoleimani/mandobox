// Command mando-gateway is the fleet host's egress proxy. It binds the host
// service anchor and is the only egress path for guests: it injects the real Anthropic key
// (host-side, never in a guest) for ANTHROPIC_BASE_URL traffic and tunnels allowlisted
// git/registry hosts for HTTPS_PROXY traffic — on one port.
//
// LiteLLM/mcp-guardian replace or front this later for model routing and richer policy; this
// minimal gateway covers the single-model path.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chelodo/mandobox/internal/gateway"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	listen := flag.String("listen", envOr("FLEET_GW_LISTEN", "172.31.0.1:8080"), "listen address (host anchor)")
	upstream := flag.String("upstream", envOr("FLEET_GW_UPSTREAM", "https://api.anthropic.com"), "LLM upstream base (LiteLLM or Anthropic)")
	keyFile := flag.String("key-file", envOr("FLEET_GW_KEY_FILE", "/etc/fleet/gateway/anthropic.key"), "file with the upstream key (LiteLLM master key or Anthropic key)")
	keyHeader := flag.String("key-header", envOr("FLEET_GW_KEY_HEADER", "X-Api-Key"), "header to inject the upstream key under (x-litellm-api-key for LiteLLM)")
	allowFile := flag.String("allowlist-file", envOr("FLEET_GW_ALLOWLIST", ""), "optional file, one host per line; defaults to the built-in list")
	mode := flag.String("mode", envOr("FLEET_GW_MODE", gateway.ModeStrict), "egress policy: strict (allowlist only) | open (any host, still logged)")
	flag.Parse()

	keyBytes, err := os.ReadFile(*keyFile)
	if err != nil {
		log.Error("read key file", "err", err)
		os.Exit(1)
	}
	allow := gateway.DefaultAllowlist()
	if *allowFile != "" {
		extra, rerr := readAllowlist(*allowFile)
		if rerr != nil {
			log.Error("read allowlist", "err", rerr)
			os.Exit(1)
		}
		allow = append(allow, extra...) // extend the curated built-in list, don't replace it
	}

	g, err := gateway.New(gateway.Config{
		UpstreamBaseURL:   *upstream,
		UpstreamKey:       strings.TrimSpace(string(keyBytes)),
		UpstreamKeyHeader: *keyHeader,
		Allowlist:         allow,
		Mode:              *mode,
		Log:               log,
	})
	if err != nil {
		log.Error("build gateway", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{Addr: *listen, Handler: g, ReadHeaderTimeout: 15 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("mando-gateway listening", "addr", *listen, "mode", *mode, "allowlist", len(allow))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func readAllowlist(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
