package fleetagent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/acme/fleet/internal/session"
)

// Server exposes the fleet-agent HTTP API. It is mTLS-only in production; the control plane
// initiates every call (PLAN §7.1). The service is thin: it validates, delegates to the
// Manager, and maps errors to status codes.
type Server struct {
	mgr *Manager
	log *slog.Logger
}

// NewServer returns a Server backed by mgr.
func NewServer(mgr *Manager, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{mgr: mgr, log: log}
}

// apiLaunchRequest mirrors POST /vms (PLAN §7.1).
type apiLaunchRequest struct {
	SessionID        string         `json:"session_id"`
	ImageSHA         string         `json:"image_sha"`
	VCPUs            int            `json:"vcpus"`
	MemMiB           int            `json:"mem_mib"`
	BootArgs         string         `json:"boot_args,omitempty"`
	WorkspaceSizeMiB int            `json:"workspace_size_mib,omitempty"`
	MMDS             map[string]any `json:"mmds_payload"`
}

// apiLaunchResponse mirrors the POST /vms result.
type apiLaunchResponse struct {
	Tap     string `json:"tap"`
	Chroot  string `json:"chroot"`
	GuestIP string `json:"guest_ip"`
	HostIP  string `json:"host_ip"`
}

// Handler returns the mux with all routes registered (Go 1.22+ method/path patterns).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /vms", s.launch)
	mux.HandleFunc("GET /vms", s.list)
	mux.HandleFunc("DELETE /vms/{id}", s.destroy)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) launch(w http.ResponseWriter, r *http.Request) {
	var req apiLaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	id, err := session.Parse(req.SessionID)
	if err != nil {
		s.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	rec, err := s.mgr.Launch(r.Context(), LaunchRequest{
		Session:          id,
		ImageSHA:         req.ImageSHA,
		VCPUs:            req.VCPUs,
		MemMiB:           req.MemMiB,
		BootArgs:         req.BootArgs,
		WorkspaceSizeMiB: req.WorkspaceSizeMiB,
		MMDS:             req.MMDS,
	})
	switch {
	case errors.Is(err, ErrAtCapacity):
		// Retryable: the LaunchVM activity backs off (PLAN §7.1, EX_TEMPFAIL semantics).
		s.fail(w, http.StatusServiceUnavailable, "at capacity")
		return
	case errors.Is(err, ErrForbiddenMMDS):
		s.fail(w, http.StatusBadRequest, "%v", err)
		return
	case err != nil:
		s.log.Error("launch failed", "session_id", id, "err", err)
		s.fail(w, http.StatusInternalServerError, "launch failed")
		return
	}
	writeJSON(w, http.StatusOK, apiLaunchResponse{
		Tap: rec.Tap, Chroot: rec.Chroot, GuestIP: rec.GuestIP, HostIP: rec.HostIP,
	})
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	id, err := session.Parse(r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	purge := false
	if v := r.URL.Query().Get("purge_workspace"); v != "" {
		if purge, err = strconv.ParseBool(v); err != nil {
			s.fail(w, http.StatusBadRequest, "purge_workspace: %v", err)
			return
		}
	}
	if err := s.mgr.Destroy(r.Context(), id, purge); err != nil {
		s.log.Error("destroy failed", "session_id", id, "err", err)
		s.fail(w, http.StatusInternalServerError, "destroy failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	vms, err := s.mgr.List()
	if err != nil {
		s.log.Error("list failed", "err", err)
		s.fail(w, http.StatusInternalServerError, "list failed")
		return
	}
	if vms == nil {
		vms = []VMRecord{}
	}
	writeJSON(w, http.StatusOK, vms)
}

func (s *Server) fail(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// LoadServerTLS builds a TLS config that requires and verifies a client certificate signed
// by the given CA (mTLS, PLAN §7.1). The control plane presents its client cert on every call.
func LoadServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load server keypair: %w", err)
	}
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("client CA %s contains no certificates", clientCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
