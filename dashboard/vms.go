package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// vmStore reads the fleet host's live VMs from mando-agent's mTLS API (GET /vms) — the same
// authoritative source the reconciler uses. It's read-only: the dashboard observes VMs, it never
// launches or destroys them.
type vmStore struct {
	fleetURL string
	certFile string
	keyFile  string
	caFile   string

	mu sync.Mutex
	hc *http.Client
}

func newVMStore(fleetURL, certFile, keyFile, caFile string) *vmStore {
	return &vmStore{fleetURL: fleetURL, certFile: certFile, keyFile: keyFile, caFile: caFile}
}

// vmRecord mirrors the fleet-agent VMRecord returned by GET /vms.
type vmRecord struct {
	Session   string `json:"session_id"`
	ImageSHA  string `json:"image_sha"`
	Tap       string `json:"tap"`
	Chroot    string `json:"chroot"`
	Workspace string `json:"workspace"`
	GuestIP   string `json:"guest_ip"`
	HostIP    string `json:"host_ip"`
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	PID       int    `json:"pid"`
	StartedAt int64  `json:"started_at"` // epoch seconds
}

// vmView is the enriched shape the UI renders: the raw record plus uptime and an orphan flag
// (a VM with no matching Running workflow — the reconciler would eventually reap it).
type vmView struct {
	vmRecord
	StartedISO   string `json:"started_iso"`
	UptimeSec    int64  `json:"uptime_sec"`
	Orphan       bool   `json:"orphan"`
	SessionKnown bool   `json:"session_known"` // whether we could confirm against Temporal
}

// orphanGraceSec keeps a just-launched VM from being flagged before its workflow has registered.
const orphanGraceSec = 120

func (s *vmStore) client() (*http.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hc != nil {
		return s.hc, nil
	}
	cert, err := tls.LoadX509KeyPair(s.certFile, s.keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert (%s): %w", s.certFile, err)
	}
	caPEM, err := os.ReadFile(s.caFile)
	if err != nil {
		return nil, fmt.Errorf("read server CA (%s): %w", s.caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server CA %s has no usable certificates", s.caFile)
	}
	s.hc = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}},
	}
	return s.hc, nil
}

func (s *vmStore) list(ctx context.Context) ([]vmRecord, error) {
	hc, err := s.client()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.fleetURL+"/vms", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s/vms: %w", s.fleetURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /vms: unexpected status %d", resp.StatusCode)
	}
	var vms []vmRecord
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, fmt.Errorf("decode /vms: %w", err)
	}
	return vms, nil
}

// enrichVMs annotates records with uptime and an orphan flag, computed against the set of session
// IDs that still have a Running workflow. If running is nil (Temporal unreachable), orphan flags
// are suppressed and SessionKnown=false so the UI doesn't cry wolf.
func enrichVMs(vms []vmRecord, running map[string]bool, now time.Time) []vmView {
	out := make([]vmView, len(vms))
	for i, vm := range vms {
		v := vmView{vmRecord: vm}
		if vm.StartedAt > 0 {
			started := time.Unix(vm.StartedAt, 0)
			v.StartedISO = started.UTC().Format(time.RFC3339)
			v.UptimeSec = int64(now.Sub(started).Seconds())
		}
		if running != nil {
			v.SessionKnown = true
			v.Orphan = !running[vm.Session] && v.UptimeSec > orphanGraceSec
		}
		out[i] = v
	}
	return out
}
