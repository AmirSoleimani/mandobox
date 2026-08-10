package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicProxyInjectsKeyAndStripsSession(t *testing.T) {
	var gotKey, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	g, err := New(Config{UpstreamBaseURL: upstream.URL, UpstreamKey: "real-key-123"})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(g)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sess-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if gotKey != "real-key-123" {
		t.Errorf("upstream X-Api-Key = %q, want the real key injected", gotKey)
	}
	if gotAuth != "" {
		t.Errorf("session token leaked upstream: Authorization = %q", gotAuth)
	}
}

func TestUpstreamKeyHeaderIsConfigurable(t *testing.T) {
	var gotLiteLLM, gotXApiKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLiteLLM = r.Header.Get("x-litellm-api-key")
		gotXApiKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	g, err := New(Config{UpstreamBaseURL: upstream.URL, UpstreamKey: "sk-litellm", UpstreamKeyHeader: "x-litellm-api-key"})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(g)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", strings.NewReader(`{}`))
	req.Header.Set("X-Api-Key", "sess-token") // guest session token — must be stripped
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if gotLiteLLM != "sk-litellm" {
		t.Errorf("x-litellm-api-key = %q, want the LiteLLM key injected", gotLiteLLM)
	}
	if gotXApiKey != "" {
		t.Errorf("guest X-Api-Key leaked upstream: %q", gotXApiKey)
	}
}

func TestAnthropicProxyRequiresToken(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", UpstreamKey: "k"})
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-token request = %d, want 401", rr.Code)
	}
}

func TestScrubResponseRedactsKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leak":"real-key-123 oops"}`))
	}))
	defer upstream.Close()

	g, _ := New(Config{UpstreamBaseURL: upstream.URL, UpstreamKey: "real-key-123"})
	front := httptest.NewServer(g)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/messages", nil)
	req.Header.Set("X-Api-Key", "sess")
	resp, _ := http.DefaultClient.Do(req)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "real-key-123") {
		t.Errorf("real key not scrubbed from response: %s", body)
	}
	if !strings.Contains(string(body), redacted) {
		t.Errorf("expected redaction marker, got: %s", body)
	}
}

func TestAllowlist(t *testing.T) {
	g, _ := New(Config{
		UpstreamBaseURL: "https://api.anthropic.com", UpstreamKey: "k",
		Allowlist: []string{"github.com", ".githubusercontent.com"},
	})
	cases := map[string]bool{
		"github.com":                    true,
		"objects.githubusercontent.com": true, // subdomain via ".githubusercontent.com"
		"githubusercontent.com":         true, // apex via ".githubusercontent.com"
		"evil.com":                      false,
		"notgithub.com":                 false,
	}
	for host, want := range cases {
		if got := g.allowed(host); got != want {
			t.Errorf("allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestConnectDenied(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", UpstreamKey: "k",
		Allowlist: []string{"github.com"}})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://evil.com:443", nil)
	req.Host = "evil.com:443"
	g.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("CONNECT to denied host = %d, want 403", rr.Code)
	}
}

// TestConnectNon443Denied: a guest must not tunnel non-HTTPS ports through an allowlisted host.
func TestConnectNon443Denied(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", UpstreamKey: "k",
		Allowlist: []string{"github.com"}, Mode: ModeOpen})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://github.com:22", nil)
	req.Host = "github.com:22"
	g.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("CONNECT github.com:22 = %d, want 403 (port pin)", rr.Code)
	}
}

// TestConnectInternalDeniedOpenMode: even in ModeOpen, a guest cannot pivot to host-internal targets.
func TestConnectInternalDeniedOpenMode(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", UpstreamKey: "k", Mode: ModeOpen})
	for _, target := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:443", "172.31.0.1:443", "192.168.1.5:443", "10.0.0.1:443"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodConnect, "http://"+target, nil)
		req.Host = target
		g.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("ModeOpen CONNECT %s = %d, want 403 (internal-IP guard)", target, rr.Code)
		}
	}
}

func TestIsInternalIP(t *testing.T) {
	internal := []string{"127.0.0.1", "::1", "169.254.169.254", "172.31.0.1", "10.1.2.3", "192.168.0.1", "0.0.0.0", "fe80::1"}
	external := []string{"140.82.121.3", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, s := range internal {
		if !isInternalIP(net.ParseIP(s)) {
			t.Errorf("isInternalIP(%s) = false, want true", s)
		}
	}
	for _, s := range external {
		if isInternalIP(net.ParseIP(s)) {
			t.Errorf("isInternalIP(%s) = true, want false", s)
		}
	}
}

// TestInferencePathAllowlist: the LLM reverse-proxy admits only inference routes, so a guest can't
// reach LiteLLM's admin surface with the injected master key.
func TestInferencePathAllowlist(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://up.example", UpstreamKey: "k"})
	deny := []struct {
		m, p string
	}{
		{http.MethodPost, "/model/new"}, {http.MethodGet, "/model/info"}, {http.MethodGet, "/key/info"},
		{http.MethodPost, "/key/generate"}, {http.MethodGet, "/config/yaml"}, {http.MethodGet, "/spend/logs"},
		{http.MethodPost, "/user/new"}, {http.MethodGet, "/health"}, {http.MethodGet, "/v1/messages"},
	}
	for _, c := range deny {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(c.m, c.p, nil)
		req.Header.Set("X-Api-Key", "sess")
		g.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 (not an inference route)", c.m, c.p, rr.Code)
		}
	}
	// Allowed routes must NOT be 403 (they'll fail to dial up.example, but must pass the allowlist).
	for _, c := range []struct{ m, p string }{{http.MethodPost, "/v1/messages"}, {http.MethodPost, "/v1/messages/count_tokens"}, {http.MethodPost, "/v1/chat/completions"}, {http.MethodGet, "/v1/models"}} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(c.m, c.p, nil)
		req.Header.Set("X-Api-Key", "sess")
		g.ServeHTTP(rr, req)
		if rr.Code == http.StatusForbidden {
			t.Errorf("%s %s = 403, want it allowed through the path guard", c.m, c.p)
		}
	}
}
