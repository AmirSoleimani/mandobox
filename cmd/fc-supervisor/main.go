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

	"github.com/acme/mandobox/internal/supervisor"
)

func main() {
	// As PID 1 the kernel gives us no environment, so set a PATH before running anything —
	// otherwise ip/git/gh/claude and the tools the agent shells out to are unfindable.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/go/bin")
	}
	if os.Getenv("HOME") == "" {
		_ = os.Setenv("HOME", "/root")
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx := context.Background()

	runner := supervisor.NewExecRunner()
	platform := supervisor.NewPlatform()

	cfg, err := supervisor.Bootstrap(ctx, platform, runner, "")
	if err != nil {
		fatal(log, platform, "bootstrap", err)
	}
	log.Info("booted", "session_id", cfg.SessionID, "repo", cfg.Repo.Slug, "mode", cfg.Task.Mode)

	// Adopt the hostname the VS Code tunnel token was minted under. The CLI binds its stored auth
	// to the hostname, so without this a human attach falls back to the GitHub device login even
	// though the token is present (§remote-attach). Writing the kernel hostname via /proc is
	// equivalent to sethostname(2) and keeps this file OS-portable to compile. No-op when unset.
	if h := cfg.VSCode.Hostname; h != "" {
		if err := os.WriteFile("/proc/sys/kernel/hostname", []byte(h), 0o644); err != nil {
			log.Warn("set hostname", "name", h, "err", err)
		}
	}

	// Route git/gh through the host egress gateway (the only sanctioned egress path). MMDS is
	// already read above; NATS uses a raw TCP connection unaffected by these. NO_PROXY keeps
	// MMDS and loopback direct.
	_ = os.Setenv("HTTPS_PROXY", cfg.LLM.BaseURL)
	_ = os.Setenv("HTTP_PROXY", cfg.LLM.BaseURL)
	_ = os.Setenv("NO_PROXY", "169.254.169.254,localhost,127.0.0.1")

	if err := supervisor.ConfigureNetwork(ctx, runner, cfg.Network, "/etc/resolv.conf"); err != nil {
		fatal(log, platform, "network", err)
	}

	// Refuse to run without transport: no NATS means nothing can see or stop this VM (§8.1).
	transport, err := supervisor.DialNATS(cfg.NATS.URL, cfg.NATS.Creds)
	if err != nil {
		fatal(log, platform, "nats", err)
	}
	bus := supervisor.NewBus(transport, cfg.SessionID)

	// Pick the coding-agent harness from the resolved config (docs/configuration.md). Default Claude.
	var agent supervisor.AgentRunner = supervisor.NewClaudeRunner()
	if cfg.Agent.Harness == "codex" {
		agent = supervisor.NewCodexRunner()
		log.Info("agent harness", "harness", "codex")
	}

	sup := supervisor.New(cfg, supervisor.Deps{
		Bus:      bus,
		Runner:   runner,
		Agent:    agent,
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
