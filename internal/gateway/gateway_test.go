package gateway

import (
	"io"
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

	g, err := New(Config{UpstreamBaseURL: upstream.URL, AnthropicKey: "real-key-123"})
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

func TestAnthropicProxyRequiresToken(t *testing.T) {
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", AnthropicKey: "k"})
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

	g, _ := New(Config{UpstreamBaseURL: upstream.URL, AnthropicKey: "real-key-123"})
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
		UpstreamBaseURL: "https://api.anthropic.com", AnthropicKey: "k",
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
	g, _ := New(Config{UpstreamBaseURL: "https://api.anthropic.com", AnthropicKey: "k",
		Allowlist: []string{"github.com"}})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodConnect, "http://evil.com:443", nil)
	req.Host = "evil.com:443"
	g.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("CONNECT to denied host = %d, want 403", rr.Code)
	}
}
