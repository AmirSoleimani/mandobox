package supervisor

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// The human-attach tunnel (`code tunnel`) lets a trusted operator open a browser VS Code into this
// live VM to look around or edit by hand. It is started and stopped by control-plane commands
// (CommandAttach / CommandDetach) — never by the agent, since standing up remote access is an
// operator action. While it runs, the supervisor does not self-park (a human is inside).

// startTunnel launches `code tunnel` in the background if it isn't already running. Its output
// lines (the GitHub device-login prompt, then the vscode.dev URL) are relayed as EventTunnel so the
// control plane can post them to the operator's thread. Idempotent.
func (s *Supervisor) startTunnel() {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if s.tunnelOn {
		return
	}
	tctx, cancel := context.WithCancel(s.runCtx)
	s.tunnelCancel = cancel
	s.tunnelOn = true
	s.tunnelWG.Add(1)
	go s.runTunnel(tctx, tunnelName(s.cfg.SessionID.String()))
}

// tunnelName derives a VS Code-tunnel-legal name from the session id: names must be lowercase,
// alphanumeric (+ hyphens), 2–20 chars. We keep the tail of the id (its random part) for
// uniqueness. code tunnel must accept the name verbatim, or the URL we build from it won't resolve.
func tunnelName(sessionID string) string {
	var chars []rune
	for _, r := range strings.ToLower(sessionID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			chars = append(chars, r)
		}
	}
	if len(chars) > 14 {
		chars = chars[len(chars)-14:]
	}
	return "mando" + string(chars)
}

// tunnelURLField returns the leading URL token of s — everything up to the first space or control
// byte (e.g. an ANSI colour reset code tunnel may append) — with trailing sentence punctuation
// trimmed. It isolates the bare https://vscode.dev/tunnel/… URL from code tunnel's decorated line.
func tunnelURLField(s string) string {
	if i := strings.IndexFunc(s, func(r rune) bool { return r <= ' ' }); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, ".,")
}

func (s *Supervisor) runTunnel(ctx context.Context, name string) {
	defer s.tunnelWG.Done()
	defer func() {
		s.tunnelMu.Lock()
		s.tunnelOn = false
		s.tunnelMu.Unlock()
	}()

	cmd := exec.CommandContext(ctx, "code", "tunnel",
		"--accept-server-license-terms", "--cli-data-dir", s.vscodeDataDir(), "--name", name)
	cmd.Dir = s.repoDir // open the tunnel on the repo so vscode.dev lands in the working tree
	cmd.Env = s.tunnelEnv()
	cmd.Stderr = cmd.Stdout // fold stderr in — code tunnel prints the prompt across both
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = s.deps.Bus.Event(Event{Type: EventTunnel, Info: "could not start tunnel: " + err.Error()})
		return
	}
	if err := cmd.Start(); err != nil {
		_ = s.deps.Bus.Event(Event{Type: EventTunnel, Info: "could not start tunnel: " + err.Error()})
		return
	}
	s.deps.Log.Info("code tunnel started", "name", name)

	var mu sync.Mutex
	var announced []string
	seen := map[string]bool{}
	// surface posts a tunnel line once and remembers it so reAnnounce can re-emit it for a late relay
	// (NATS core drops messages for subscribers that weren't listening yet); RelayTunnel dedupes too.
	surface := func(info string) {
		mu.Lock()
		if seen[info] {
			mu.Unlock()
			return
		}
		seen[info] = true
		announced = append(announced, info)
		mu.Unlock()
		_ = s.deps.Bus.Event(Event{Type: EventTunnel, Info: info})
	}
	go s.reAnnounce(ctx, &mu, &announced)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Surface only the two actionable lines. The vscode.dev URL is taken from code tunnel's own
		// output (not constructed up front) because that line appears only after the tunnel is
		// registered and reachable — so the link works the instant it's clicked, instead of racing
		// registration and showing "could not find tunnel" until a refresh. It already carries the
		// /workspace/repo folder (cmd.Dir). The device-login prompt is the fallback for a VM that
		// wasn't pre-authed; a pre-authed VM never prints it. Everything else is license/info noise.
		if i := strings.Index(line, "https://vscode.dev/tunnel/"); i >= 0 {
			surface("Open VS Code here: " + tunnelURLField(line[i:]))
		} else if strings.Contains(line, "github.com/login/device") {
			surface(line)
		}
	}
	_ = cmd.Wait()
	s.deps.Log.Info("code tunnel stopped", "name", name)
	s.unregisterTunnel(name)
}

// unregisterTunnel removes this session's tunnel from the account when it stops. VS Code caps tunnels
// at ~10 per user; without this, every attach leaves a registration behind and the 11th session's
// attach can't create one (it fails with "exceed the limit for TunnelsPerUserPerCluster"). Best-effort
// and on its own context so it still runs while the session is tearing down. No-op if none registered.
func (s *Supervisor) unregisterTunnel(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "code", "tunnel", "unregister", "--cli-data-dir", s.vscodeDataDir())
	cmd.Env = s.tunnelEnv()
	if err := cmd.Run(); err != nil {
		s.deps.Log.Warn("tunnel unregister", "name", name, "err", err)
		return
	}
	s.deps.Log.Info("tunnel unregistered", "name", name)
}

// shutdownTunnel is called on session teardown: it stops any live tunnel and waits for the goroutine
// to finish unregistering, so the account slot is released before the VM powers off.
func (s *Supervisor) shutdownTunnel() {
	s.stopTunnel() // no-op if not running; cancels the tunnel so runTunnel unregisters and exits
	done := make(chan struct{})
	go func() { s.tunnelWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(35 * time.Second):
		s.deps.Log.Warn("tunnel shutdown timed out")
	}
}

// reAnnounceInterval is how often the tunnel's URL is re-broadcast while a human is attached.
const reAnnounceInterval = 15 * time.Second

// reAnnounce re-publishes the tunnel's login/URL lines for the whole life of the attach (until the
// tunnel is stopped, when ctx is cancelled). NATS core drops messages for subscribers that aren't
// listening yet, so a relay that (re)subscribes at any point — including a fresh RelayTunnel attempt
// after a worker restart — recovers the URL within one interval instead of being stranded. The relay
// dedupes, so a steady attach still shows a single link; only a restarted relay re-posts it.
func (s *Supervisor) reAnnounce(ctx context.Context, mu *sync.Mutex, announced *[]string) {
	t := time.NewTicker(reAnnounceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mu.Lock()
			lines := append([]string(nil), (*announced)...)
			mu.Unlock()
			for _, l := range lines {
				_ = s.deps.Bus.Event(Event{Type: EventTunnel, Info: l})
			}
		}
	}
}

// stopTunnel kills the tunnel process if running.
func (s *Supervisor) stopTunnel() {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	if s.tunnelCancel != nil {
		s.tunnelCancel()
		s.tunnelCancel = nil
	}
	s.tunnelOn = false
}

// tunnelActive reports whether a human is attached (so the keep-alive loop won't park the VM).
func (s *Supervisor) tunnelActive() bool {
	s.tunnelMu.Lock()
	defer s.tunnelMu.Unlock()
	return s.tunnelOn
}

// detach stops the tunnel and reports the working-tree status, so the control plane can ask the
// operator what to do with any changes they made by hand (commit / discard / hand to the agent). The
// EventDetached is re-emitted over a short window so a relay that's mid-reconnect (e.g. just after a
// worker restart) still catches it — otherwise the retrying relay would wait for a detach it missed
// and the session would stay frozen. The relay returns on the first one it sees; the rest are no-ops.
func (s *Supervisor) detach() {
	s.stopTunnel()
	status, err := s.git.output(s.runCtx, "status", "--short")
	if err != nil {
		status = ""
	}
	ev := Event{Type: EventDetached, Info: strings.TrimSpace(status)}
	_ = s.deps.Bus.Event(ev)
	go func() {
		t := time.NewTicker(reAnnounceInterval / 3) // ~5s, spanning the relay's reconnect backoff
		defer t.Stop()
		for range 6 {
			select {
			case <-s.runCtx.Done():
				return
			case <-t.C:
				_ = s.deps.Bus.Event(ev)
			}
		}
	}()
}

// tunnelEnv routes `code tunnel` through the host egress gateway (like the agent), so its traffic
// stays inside the allowlist. The CLI's auth path is pinned separately via --cli-data-dir (see
// runTunnel), so it does not depend on HOME. Nothing here carries a Tier-0 credential.
func (s *Supervisor) tunnelEnv() []string {
	env := os.Environ()
	if gw := s.cfg.LLM.BaseURL; gw != "" {
		env = append(env,
			"HTTPS_PROXY="+gw, "HTTP_PROXY="+gw,
			"NO_PROXY=169.254.169.254,localhost,127.0.0.1")
	}
	return env
}
