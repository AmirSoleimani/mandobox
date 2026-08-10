package supervisor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// mmdsAddr is Firecracker's MMDS link-local address.
const mmdsAddr = "http://169.254.169.254"

// MMDSClient reads the guest metadata service using MMDS V2: obtain a session token, then
// GET the tree with it. V2's token requirement blocks casual SSRF-shaped reads; it does not
// change the threat model — anything in the guest can still read MMDS, so everything in it
// expires within an hour.
type MMDSClient struct {
	base   string
	http   *http.Client
	ttlSec int
}

// NewMMDSClient returns a client for the given base URL (default the MMDS link-local).
func NewMMDSClient(base string) *MMDSClient {
	if base == "" {
		base = mmdsAddr
	}
	return &MMDSClient{
		base:   base,
		http:   &http.Client{Timeout: 5 * time.Second},
		ttlSec: 21600,
	}
}

// token obtains an MMDS V2 session token.
func (c *MMDSClient) token(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-metadata-token-ttl-seconds", strconv.Itoa(c.ttlSec))
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("mmds token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("mmds token: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", fmt.Errorf("mmds token: read: %w", err)
	}
	return string(body), nil
}

// Fetch returns the raw MMDS JSON tree.
func (c *MMDSClient) Fetch(ctx context.Context) ([]byte, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-metadata-token", tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mmds fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mmds fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("mmds fetch: read: %w", err)
	}
	return body, nil
}
