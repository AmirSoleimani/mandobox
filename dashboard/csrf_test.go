package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSameOriginGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := sameOrigin(ok)
	do := func(method, host, sfs, ct string) int {
		r := httptest.NewRequest(method, "/api/secrets/rotate", nil)
		r.Host = host
		if sfs != "" {
			r.Header.Set("Sec-Fetch-Site", sfs)
		}
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	cases := []struct {
		name                  string
		method, host, sfs, ct string
		want                  int
	}{
		{"GET local passes", "GET", "localhost:8087", "", "", 200},
		{"same-origin json POST passes", "POST", "localhost:8087", "same-origin", "application/json", 200},
		{"curl json POST (no sec-fetch) passes", "POST", "127.0.0.1:8087", "", "application/json", 200},
		{"cross-site POST blocked", "POST", "localhost:8087", "cross-site", "application/json", 403},
		{"non-json POST blocked", "POST", "localhost:8087", "", "text/plain", 415},
		{"form POST blocked", "POST", "localhost:8087", "", "application/x-www-form-urlencoded", 415},
		{"DNS-rebinding host blocked", "POST", "evil.com", "same-origin", "application/json", 403},
		{"rebinding host blocked on GET too", "GET", "attacker.example", "", "", 403},
	}
	for _, c := range cases {
		if got := do(c.method, c.host, c.sfs, c.ct); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
