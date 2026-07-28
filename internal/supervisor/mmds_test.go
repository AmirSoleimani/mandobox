package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMMDSFetchV2(t *testing.T) {
	const token = "TESTTOKEN"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-metadata-token-ttl-seconds") == "" {
				t.Error("token request missing TTL header")
			}
			_, _ = w.Write([]byte(token))
		case r.Method == http.MethodGet && r.URL.Path == "/":
			if r.Header.Get("X-metadata-token") != token {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"session_id":"s_0123456789ABCDEFGHJKMNPQRS"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewMMDSClient(srv.URL)
	body, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(body) != `{"session_id":"s_0123456789ABCDEFGHJKMNPQRS"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestMMDSFetchTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewMMDSClient(srv.URL).Fetch(context.Background()); err == nil {
		t.Fatal("expected error when token endpoint fails")
	}
}
