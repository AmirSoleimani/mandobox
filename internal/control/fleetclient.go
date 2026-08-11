package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/reconcile"
)

// FleetClient talks to mando-agent's mTLS HTTP API. It adds Launch (POST /vms) on
// top of the List/Destroy already in internal/reconcile, reusing that package's TLS loader.
type FleetClient struct {
	base  string
	httpc *http.Client
}

// launchRequest mirrors fleetagent.apiLaunchRequest (server.go). MMDS carries the BootConfig
// minus network/session_id, which mando-agent fills in (a trust-boundary invariant forbids the anthropic_api_key here).
type launchRequest struct {
	SessionID        string         `json:"session_id"`
	ImageSHA         string         `json:"image_sha"`
	VCPUs            int            `json:"vcpus"`
	MemMiB           int            `json:"mem_mib"`
	BootArgs         string         `json:"boot_args,omitempty"`
	WorkspaceSizeMiB int            `json:"workspace_size_mib,omitempty"`
	MMDS             map[string]any `json:"mmds_payload"`
}

// NewFleetClient builds an mTLS client presenting the reconciler cert.
func NewFleetClient(base, certFile, keyFile, serverCAFile string) (*FleetClient, error) {
	tlsCfg, err := reconcile.LoadClientTLS(certFile, keyFile, serverCAFile)
	if err != nil {
		return nil, err
	}
	return &FleetClient{
		base:  strings.TrimRight(base, "/"),
		httpc: &http.Client{Timeout: 120 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg}},
	}, nil
}

// ErrAtCapacity is returned when mando-agent replies 503; LaunchVM retries with backoff.
var ErrAtCapacity = fmt.Errorf("mando-agent at capacity")

// Launch posts a launch request. A 503 (at capacity) is surfaced as ErrAtCapacity so the
// activity's retry policy backs off rather than failing the workflow.
func (c *FleetClient) Launch(ctx context.Context, req launchRequest) (LaunchResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return LaunchResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/vms", bytes.NewReader(body))
	if err != nil {
		return LaunchResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(httpReq)
	if err != nil {
		return LaunchResult{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusServiceUnavailable:
		return LaunchResult{}, ErrAtCapacity
	case resp.StatusCode/100 != 2:
		return LaunchResult{}, fmt.Errorf("launch: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var out LaunchResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return LaunchResult{}, fmt.Errorf("decode launch response: %w", err)
	}
	return out, nil
}

// Destroy deletes a VM; purgeWorkspace also discards the persistent volume.
// VMRecord is one running VM as returned by GET /vms. The reconcile activity needs the session and
// its start time; other fields the agent returns are ignored here.
type VMRecord struct {
	Session   string `json:"session_id"`
	StartedAt int64  `json:"started_at"` // epoch seconds
}

// List returns the fleet host's running VMs (GET /vms). Used by the reconcile activity to diff
// actual host state against Temporal's expected sessions.
func (c *FleetClient) List(ctx context.Context) ([]VMRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/vms", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return nil, fmt.Errorf("list vms: %s", resp.Status)
	}
	var vms []VMRecord
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, fmt.Errorf("decode vms: %w", err)
	}
	return vms, nil
}

func (c *FleetClient) Destroy(ctx context.Context, sessionID string, purgeWorkspace bool) error {
	url := fmt.Sprintf("%s/vms/%s?purge_workspace=%t", c.base, sessionID, purgeWorkspace)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("destroy %s: %s", sessionID, resp.Status)
	}
	return nil
}
