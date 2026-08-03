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
	Log       *slog.Logger
}

// Gateway implements the egress proxy.
type Gateway struct {
	cfg      Config
	upstream *url.URL
	allow    []string
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
	g := &Gateway{cfg: cfg, upstream: up, allow: cfg.Allowlist, log: cfg.Log}
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
	g.proxy.ServeHTTP(w, r)
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
	body, err := io.ReadAll(resp.Body)
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
	host := hostOnly(r.Host)
	if !g.allowed(host) {
		g.log.Warn("gateway: CONNECT denied", "host", host)
		http.Error(w, "destination not allowed", http.StatusForbidden)
		return
	}
	dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
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

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// DefaultAllowlist is the minimal set of hosts a coding agent needs for git and package
// fetches. Extend per your repos' registries (§7.5).
func DefaultAllowlist() []string {
	return []string{
		"github.com",
		"api.github.com",
		"codeload.github.com",
		"objects.githubusercontent.com",
		"registry.npmjs.org",
		"pypi.org",
		"files.pythonhosted.org",
		"proxy.golang.org",
		"sum.golang.org",
	}
}
