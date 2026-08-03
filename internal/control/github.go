package control

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHubApp mints short-lived installation tokens (Tier-1) from the App's private key
// (Tier-0). This is the Go port of scripts/mint-github-token.sh: an RS256 App JWT, then an
// installation-scoped access token. The private key never leaves the host (§9).
type GitHubApp struct {
	AppID          string
	InstallationID string // if empty, resolved from Org
	Org            string
	key            *rsa.PrivateKey
	httpc          *http.Client
	now            func() time.Time // injectable for tests
}

// NewGitHubApp parses the PEM private key (PKCS#1 or PKCS#8).
func NewGitHubApp(appID, org, installationID string, keyPEM []byte) (*GitHubApp, error) {
	key, err := parseRSAKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse app key: %w", err)
	}
	return &GitHubApp{
		AppID:          appID,
		InstallationID: installationID,
		Org:            org,
		key:            key,
		httpc:          &http.Client{Timeout: 20 * time.Second},
		now:            time.Now,
	}, nil
}

func parseRSAKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an RSA private key")
	}
	return rk, nil
}

// appJWT builds the RS256 App JWT: iat backdated 60s, exp now+9m, iss=AppID (§9, GitHub caps
// the lifetime at 10m).
func (g *GitHubApp) appJWT() (string, error) {
	now := g.now()
	header := b64url(`{"alg":"RS256","typ":"JWT"}`)
	payload := b64url(fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":"%s"}`,
		now.Add(-60*time.Second).Unix(), now.Add(9*time.Minute).Unix(), g.AppID))
	signingInput := header + "." + payload

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// MintInstallationToken returns a ~1h installation token scoped to the App's installation.
func (g *GitHubApp) MintInstallationToken(ctx context.Context) (string, error) {
	jwt, err := g.appJWT()
	if err != nil {
		return "", err
	}
	instID := g.InstallationID
	if instID == "" {
		instID, err = g.resolveInstallationID(ctx, jwt)
		if err != nil {
			return "", err
		}
	}
	var out struct {
		Token string `json:"token"`
	}
	url := "https://api.github.com/app/installations/" + instID + "/access_tokens"
	if err := g.apiJSON(ctx, http.MethodPost, url, jwt, &out); err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("empty installation token")
	}
	return out.Token, nil
}

func (g *GitHubApp) resolveInstallationID(ctx context.Context, jwt string) (string, error) {
	if g.Org == "" {
		return "", fmt.Errorf("neither InstallationID nor Org set")
	}
	var out struct {
		ID int64 `json:"id"`
	}
	url := "https://api.github.com/orgs/" + g.Org + "/installation"
	if err := g.apiJSON(ctx, http.MethodGet, url, jwt, &out); err != nil {
		return "", fmt.Errorf("resolve installation: %w", err)
	}
	if out.ID == 0 {
		return "", fmt.Errorf("no installation for org %q", g.Org)
	}
	return strconv.FormatInt(out.ID, 10), nil
}

// FindOpenPRByBranch returns the open PR whose head is `branch` in `repo` (owner/name), or
// (0, "") if none. Used to reconcile after a run whose pr_opened event was lost — NATS is
// at-most-once, so a real PR must never be mistaken for "no PR" (§6).
func (g *GitHubApp) FindOpenPRByBranch(ctx context.Context, repo, branch string) (int, string, error) {
	token, err := g.MintInstallationToken(ctx)
	if err != nil {
		return 0, "", err
	}
	owner, _, _ := strings.Cut(repo, "/")
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls?state=open&head=%s:%s&per_page=1",
		repo, owner, branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return 0, "", fmt.Errorf("list prs: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var prs []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &prs); err != nil {
		return 0, "", err
	}
	if len(prs) == 0 {
		return 0, "", nil
	}
	return prs[0].Number, prs[0].HTMLURL, nil
}

func (g *GitHubApp) apiJSON(ctx context.Context, method, url, jwt string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
