package fleetagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chelodo/fleet/internal/session"
)

func newTestServer(t *testing.T, maxVMs int) (http.Handler, *Manager) {
	t.Helper()
	m, _, _ := newTestManager(t, maxVMs)
	return NewServer(m, nil).Handler(), m
}

func TestHealthz(t *testing.T) {
	h, _ := newTestServer(t, 8)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rr.Code)
	}
}

func TestLaunchAPI(t *testing.T) {
	h, m := newTestServer(t, 8)
	body := `{"session_id":"s_0123456789ABCDEFGHJKMNPQRS","image_sha":"deadbeefcafebabe","vcpus":2,"mem_mib":4096,"mmds_payload":{"repo":"x"}}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("launch = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var resp apiLaunchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Tap == "" || resp.GuestIP != "172.16.0.2" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if _, err := m.store.Get(session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")); err != nil {
		t.Fatalf("state not persisted: %v", err)
	}
}

func TestLaunchAPIBadSession(t *testing.T) {
	h, _ := newTestServer(t, 8)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(`{"session_id":"nope"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad session launch = %d, want 400", rr.Code)
	}
}

func TestLaunchAPICapacity(t *testing.T) {
	h, _ := newTestServer(t, 1)
	first := `{"session_id":"s_00000000000000000000000000","image_sha":"deadbeefcafebabe","mmds_payload":{}}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(first)))
	if rr.Code != http.StatusOK {
		t.Fatalf("first launch = %d, want 200", rr.Code)
	}
	second := `{"session_id":"s_11111111111111111111111111","image_sha":"deadbeefcafebabe","mmds_payload":{}}`
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/vms", strings.NewReader(second)))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("capacity launch = %d, want 503", rr.Code)
	}
}

func TestDestroyAndListAPI(t *testing.T) {
	h, m := newTestServer(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	mustLaunch(t, m, id)

	// GET /vms lists it.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/vms", nil))
	var vms []VMRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &vms); err != nil || len(vms) != 1 {
		t.Fatalf("list = %s (%d entries) err=%v", rr.Body.String(), len(vms), err)
	}

	// DELETE removes it.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vms/"+id.String()+"?purge_workspace=false", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("destroy = %d, want 204", rr.Code)
	}
	if _, err := m.store.Get(id); !isNotFound(err) {
		t.Fatalf("state survived destroy: %v", err)
	}
}

func isNotFound(err error) bool { return err == ErrNotFound }

func TestDestroyBadPurgeParam(t *testing.T) {
	h, m := newTestServer(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	mustLaunch(t, m, id)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/vms/"+id.String()+"?purge_workspace=maybe", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad purge param = %d, want 400", rr.Code)
	}
}
