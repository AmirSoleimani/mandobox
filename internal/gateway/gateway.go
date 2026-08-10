// Package gateway is the fleet host's egress proxy (PLAN §7.5, §10): the ONLY path out of a
// guest for LLM and git/registry traffic. It injects the real Anthropic key so the guest
// holds only a per-session bearer token (I1), enforces a domain allowlist on tunnelled
// traffic, and scrubs the real key from responses. It is not a boundary on its own — the
// allowlist is a speed bump, not a wall (§4.5) — but combined with nftables it is the seam
// where credentials are added.
//
// One port serves both roles (§10): CONNECT requests are the HTTPS_PROXY forward path;
// everything else is the ANTHROPIC_BASE_URL reverse proxy to Anthropic.
package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

const redacted = "[REDACTED-BY-FLEET-GATEWAY]"

// maxScrubBytes bounds the buffered non-streaming response the gateway reads to scrub the upstream
// key. Real LLM non-streaming bodies (errors, token counts, model lists) are KB-sized.
const maxScrubBytes = 32 << 20 // 32 MiB

// Config configures the gateway.
type Config struct {
	// UpstreamBaseURL is the LLM upstream — LiteLLM (http://127.0.0.1:4000) since M5, or the
	// Anthropic API directly (https://api.anthropic.com) for the single-model path.
	UpstreamBaseURL string
	// UpstreamKey is the credential injected host-side toward the upstream — the LiteLLM master
	// key, or the real Anthropic key — never present in a guest (I1).
	UpstreamKey string
	// UpstreamKeyHeader is the header the upstream key is injected under. Default "X-Api-Key"
	// (Anthropic). LiteLLM's unambiguous proxy-auth header is "x-litellm-api-key".
	UpstreamKeyHeader string
	// Allowlist is the set of hostnames a guest may CONNECT to (git/registry egress).
	// Entries beginning with "." match that domain and any subdomain.
	Allowlist []string
	// Mode selects the egress policy: ModeStrict (default) only permits CONNECT to Allowlist
	// hosts; ModeOpen permits any host but still logs every one, so the gateway stays the single
	// audited chokepoint (nftables forces all egress through it either way). ModeOpen trades
	// exfil-prevention for zero-maintenance coverage — right for trusted repos (§4.5).
	Mode string
	Log  *slog.Logger
}

// Egress policy modes.
const (
	ModeStrict = "strict" // default-deny: only Allowlist hosts may CONNECT
	ModeOpen   = "open"   // allow-all egress, still logged (chokepoint kept for audit)
)

// Gateway implements the egress proxy.
type Gateway struct {
	cfg      Config
	upstream *url.URL
	allow    []string
	mode     string
	proxy    *httputil.ReverseProxy
	log      *slog.Logger
}

// New builds a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.UpstreamKey == "" {
		return nil, fmt.Errorf("gateway: UpstreamKey is required")
	}
	if cfg.UpstreamKeyHeader == "" {
		cfg.UpstreamKeyHeader = "X-Api-Key"
	}
	up, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil || up.Host == "" {
		return nil, fmt.Errorf("gateway: invalid UpstreamBaseURL %q", cfg.UpstreamBaseURL)
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	mode := cfg.Mode
	if mode != ModeOpen {
		mode = ModeStrict
	}
	g := &Gateway{cfg: cfg, upstream: up, allow: cfg.Allowlist, mode: mode, log: cfg.Log}
	g.proxy = &httputil.ReverseProxy{
		Director:       g.director,
		ModifyResponse: g.scrubResponse,
		ErrorLog:       slog.NewLogLogger(cfg.Log.Handler(), slog.LevelWarn),
		FlushInterval:  -1, // flush SSE frames immediately (Claude Code streams) (§10)
	}
	return g, nil
}

// ServeHTTP routes CONNECT (forward proxy) vs the Anthropic reverse proxy.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		g.handleConnect(w, r)
		return
	}
	g.handleAnthropic(w, r)
}

// handleAnthropic validates the per-session bearer token, strips it, injects the real key,
// and reverse-proxies to Anthropic.
func (g *Gateway) handleAnthropic(w http.ResponseWriter, r *http.Request) {
	if sessionToken(r) == "" {
		http.Error(w, "missing session token", http.StatusUnauthorized)
		return
	}
	// Path allowlist BEFORE the director injects the Tier-0 upstream (LiteLLM master) key: confine the
	// guest to the inference endpoints the agents use, so it can never reach LiteLLM's key/model/config
	// admin surface with master authority (confused-deputy). Default-deny everything else.
	if !inferenceAllowed(r) {
		g.log.Warn("gateway: LLM endpoint denied", "method", r.Method, "path", r.URL.Path)
		http.Error(w, "endpoint not allowed", http.StatusForbidden)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// inferenceAllowed permits only the LLM inference routes Claude Code / Codex actually call, so the
// master-key-authenticated upstream never sees an admin/config/key request from a guest.
func inferenceAllowed(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && (p == "/v1/messages" || p == "/v1/messages/count_tokens"):
		return true // Claude Code (Anthropic Messages API)
	case r.Method == http.MethodPost && (p == "/v1/chat/completions" || p == "/v1/completions" || p == "/v1/responses"):
		return true // Codex / OpenAI-compatible
	case r.Method == http.MethodGet && p == "/v1/models":
		return true // model listing (read-only)
	default:
		return false
	}
}

// director rewrites the outbound request onto the upstream and swaps the guest's session token
// for the real upstream credential (§4.5).
func (g *Gateway) director(r *http.Request) {
	r.URL.Scheme = g.upstream.Scheme
	r.URL.Host = g.upstream.Host
	r.Host = g.upstream.Host
	// Strip every credential the guest might carry, then inject the real one under the
	// configured header. Deleting the target header too prevents a guest from smuggling one in.
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")
	r.Header.Del(g.cfg.UpstreamKeyHeader)
	r.Header.Set(g.cfg.UpstreamKeyHeader, g.cfg.UpstreamKey)
	if r.Header.Get("Anthropic-Version") == "" {
		r.Header.Set("Anthropic-Version", "2023-06-01")
	}
}

// scrubResponse removes the real key from responses before they reach the guest (§7.5,
// transcript leakage §9). Streaming bodies are left intact except for buffered small bodies.
func (g *Gateway) scrubResponse(resp *http.Response) error {
	// Only scrub non-streaming bodies; SSE streams are passed through (the key is never in a
	// model response body, this guards against accidental echoes in errors).
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return nil
	}
	// Bound the buffered read: streaming completions are passed through above, so a non-streaming body
	// here is an error/token-count/models response (KB-sized). The cap stops a pathological upstream
	// body from exhausting gateway memory while still covering every real response.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxScrubBytes))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	if g.cfg.UpstreamKey != "" {
		body = bytes.ReplaceAll(body, []byte(g.cfg.UpstreamKey), []byte(redacted))
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	return nil
}

// handleConnect tunnels an allowlisted HTTPS CONNECT (git/registry egress).
func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "malformed CONNECT target", http.StatusBadRequest)
		return
	}
	// Pin to 443: a guest must not tunnel SSH/DB/arbitrary ports through an allowlisted host (e.g.
	// CONNECT github.com:22). All intended egress (git/registry/tunnel) is HTTPS.
	if port != "443" {
		g.log.Warn("gateway: CONNECT denied (non-443 port)", "host", host, "port", port)
		http.Error(w, "only port 443 permitted", http.StatusForbidden)
		return
	}
	if g.mode != ModeOpen && !g.allowed(host) {
		g.log.Warn("gateway: CONNECT denied", "host", host, "mode", g.mode)
		http.Error(w, "destination not allowed", http.StatusForbidden)
		return
	}
	// Internal-destination guard — runs in BOTH modes (ModeOpen skips the allowlist, so without this a
	// guest could CONNECT to 127.0.0.1 [the root dashboard], the fleet anchor 172.31.0.1, MMDS, other
	// guests, or the host LAN). Resolve once and dial the VETTED IP so a DNS rebind between check and
	// dial can't slip an internal address in.
	ip, err := resolvePublic(host)
	if err != nil {
		g.log.Warn("gateway: CONNECT denied (internal/unresolvable)", "host", host, "err", err.Error())
		http.Error(w, "destination not allowed", http.StatusForbidden)
		return
	}
	// Log every permitted egress: in open mode this is the audit trail that justifies keeping all
	// traffic flowing through the gateway rather than dropping the chokepoint entirely.
	g.log.Info("gateway: CONNECT", "host", host, "mode", g.mode)
	dst, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), 10*time.Second)
	if err != nil {
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		_ = dst.Close()
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		_ = dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go pipe(dst, src)
	go pipe(src, dst)
}

// allowed reports whether host is permitted for CONNECT.
func (g *Gateway) allowed(host string) bool {
	host = strings.ToLower(host)
	for _, a := range g.allow {
		a = strings.ToLower(a)
		if strings.HasPrefix(a, ".") {
			if host == a[1:] || strings.HasSuffix(host, a) {
				return true
			}
			continue
		}
		if host == a {
			return true
		}
	}
	return false
}

// resolvePublic resolves host and returns one vetted PUBLIC IP, or an error if the host is
// unresolvable or ANY of its addresses is internal (loopback / link-local / private / unspecified).
// Rejecting the whole host when any address is internal defends against DNS rebinding and split-horizon
// tricks; the caller dials the returned IP directly so no second, unchecked resolution happens.
func resolvePublic(host string) (net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses for %q", host)
	}
	for _, a := range addrs {
		if isInternalIP(a.IP) {
			return nil, fmt.Errorf("internal address %s for %q", a.IP, host)
		}
	}
	return addrs[0].IP, nil
}

// isInternalIP reports whether ip is one a guest must never reach through the gateway: the host
// itself (loopback), MMDS + IPv6 link-local (link-local), the fleet anchor + other guests + the LAN
// (private), or 0.0.0.0/:: (unspecified).
func isInternalIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified()
}

func pipe(dst io.WriteCloser, src io.Reader) {
	defer func() { _ = dst.Close() }()
	_, _ = io.Copy(dst, src)
}

// sessionToken extracts the guest's bearer token from either header Claude Code may use.
func sessionToken(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return ""
}

// DefaultAllowlist is a curated set of mainstream git hosts and package registries — enough to
// build/test the large majority of real repos in ModeStrict without per-repo tuning. Extend it
// (or the -allowlist-file) for private/niche registries, or run ModeOpen for trusted repos (§7.5).
// A "." prefix matches the domain and all subdomains.
func DefaultAllowlist() []string {
	return []string{
		// git hosts
		"github.com", "api.github.com", "codeload.github.com", "objects.githubusercontent.com",
		".githubusercontent.com", "gitlab.com", "bitbucket.org",
		// Anthropic API — reached directly by the guest ONLY in subscription auth mode
		// (docs/subscription-auth.md). Unused in the default gateway path, harmless to allow.
		"api.anthropic.com",
		// JavaScript: npm, yarn, pnpm(npm), plus the common CDNs postinstall scripts use
		"registry.npmjs.org", "registry.yarnpkg.com", "cdn.jsdelivr.net", "unpkg.com",
		// Python
		"pypi.org", "files.pythonhosted.org",
		// Go modules
		"proxy.golang.org", "sum.golang.org",
		// Rust
		"crates.io", "static.crates.io", "index.crates.io",
		// Ruby
		"rubygems.org", "index.rubygems.org",
		// Java/Kotlin (Maven Central, Gradle)
		"repo1.maven.org", "repo.maven.apache.org", ".gradle.org",
		// .NET
		"api.nuget.org",
		// PHP (Composer)
		"packagist.org", "repo.packagist.org",
		// OS package mirrors (apt), so `apt-get install` works when a build needs a system lib
		"deb.debian.org", "security.debian.org", "archive.ubuntu.com", "security.ubuntu.com", "ports.ubuntu.com",
		// VS Code remote tunnel (`code tunnel`, human attach): CLI/server download, the tunnel
		// relay, and the browser landing page. Auth uses github.com (already allowed above).
		"update.code.visualstudio.com", "vscode.download.prss.microsoft.com", ".vo.msecnd.net",
		".tunnels.api.visualstudio.com", "vscode.dev",
	}
}
