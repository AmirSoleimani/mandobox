// Command fc-supervisor is the guest's PID 1 (PLAN §8.1). It reads its boot config from
// MMDS, refuses to run without NATS (no transport means no observability or abort channel),
// runs Claude Code against the target repo behind the host LLM gateway, and turns the result
// into a pull request. It holds only per-session Tier-1 tokens; ANTHROPIC_API_KEY is never
// set (I1, I9).
//
// Build static: CGO_ENABLED=0. It is Linux-only at runtime (mounts, reboot).
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/acme/fleet/internal/supervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	runner := supervisor.NewExecRunner()
	platform := supervisor.NewPlatform()

	cfg, err := supervisor.Bootstrap(ctx, platform, runner, "")
	if err != nil {
		fatal(log, platform, "bootstrap", err)
	}
	log.Info("booted", "session_id", cfg.SessionID, "repo", cfg.Repo.Slug, "mode", cfg.Task.Mode)

	if err := supervisor.ConfigureNetwork(ctx, runner, cfg.Network, "/etc/resolv.conf"); err != nil {
		fatal(log, platform, "network", err)
	}

	// Refuse to run without transport: no NATS means nothing can see or stop this VM (§8.1).
	transport, err := supervisor.DialNATS(cfg.NATS.URL, cfg.NATS.Creds)
	if err != nil {
		fatal(log, platform, "nats", err)
	}
	bus := supervisor.NewBus(transport, cfg.SessionID)

	sup := supervisor.New(cfg, supervisor.Deps{
		Bus:      bus,
		Runner:   runner,
		Agent:    supervisor.NewClaudeRunner(),
		Platform: platform,
		Log:      log,
	}, "/workspace")

	if err := sup.Run(ctx); err != nil {
		log.Error("run failed", "err", err)
	}
	_ = bus.Close()

	// Sync and power the microVM off cleanly (§8.1).
	platform.Sync()
	if err := platform.PowerOff(); err != nil {
		log.Error("power off", "err", err)
		os.Exit(1)
	}
}

// fatal logs, powers the VM off, and exits. Used before a transport exists, so there is
// nothing to report to — the workflow's timeouts and the reaper handle the dead VM.
func fatal(log *slog.Logger, platform supervisor.Platform, stage string, err error) {
	log.Error("fatal", "stage", stage, "err", err)
	platform.Sync()
	_ = platform.PowerOff()
	os.Exit(1)
}
