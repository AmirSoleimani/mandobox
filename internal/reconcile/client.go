package reconcile

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/chelodo/fleet/internal/session"
)

// HTTPClient is a FleetClient talking to a fleet-agent over mTLS.
type HTTPClient struct {
	base string
	hc   *http.Client
}

// NewHTTPClient returns a client for the fleet-agent at base (e.g. https://host:9443) using
// tlsCfg for mTLS.
func NewHTTPClient(base string, tlsCfg *tls.Config) *HTTPClient {
	return &HTTPClient{
		base: base,
		hc: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}
}

// List fetches the fleet host's running VMs.
func (c *HTTPClient) List(ctx context.Context) ([]VM, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/vms", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list vms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError("list vms", resp)
	}
	var vms []VM
	if err := json.NewDecoder(resp.Body).Decode(&vms); err != nil {
		return nil, fmt.Errorf("decode vms: %w", err)
	}
	return vms, nil
}

// Destroy asks the fleet host to tear down a VM.
func (c *HTTPClient) Destroy(ctx context.Context, id session.ID, purgeWorkspace bool) error {
	url := fmt.Sprintf("%s/vms/%s?purge_workspace=%t", c.base, id, purgeWorkspace)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("destroy vm: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return statusError("destroy vm", resp)
	}
	return nil
}

func statusError(what string, resp *http.Response) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("%s: status %d: %s", what, resp.StatusCode, msg)
}

// LoadClientTLS builds a client TLS config presenting the given client certificate and
// trusting server certificates signed by serverCAFile (mTLS to fleet-agent).
func LoadClientTLS(certFile, keyFile, serverCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client keypair: %w", err)
	}
	caPEM, err := os.ReadFile(serverCAFile)
	if err != nil {
		return nil, fmt.Errorf("read server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("server CA %s contains no certificates", serverCAFile)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
