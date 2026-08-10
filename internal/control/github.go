package control

import (
	"bytes"
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
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitHubApp mints short-lived installation tokens (Tier-1) from the App's private key
// (Tier-0). This is the Go port of scripts/mint-github-token.sh: an RS256 App JWT, then an
// installation-scoped access token. The private key never leaves the host.
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

// appJWT builds the RS256 App JWT: iat backdated 60s, exp now+9m, iss=AppID (GitHub caps
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

// MintInstallationToken returns a ~1h token for the WHOLE installation (every repo, all granted
// permissions). Prefer MintRepoToken for anything touching one repo — a token handed to an untrusted
// guest must never reach another repo in the installation (least privilege).
func (g *GitHubApp) MintInstallationToken(ctx context.Context) (string, error) {
	return g.mintToken(ctx, "", nil)
}

// MintRepoToken returns a ~1h installation token scoped to a SINGLE repo ("owner/name") with only the
// requested permissions (e.g. {"contents":"write","pull_requests":"write"}). This confines the token
// so a malicious target repo's guest can't pivot to any other repo in the org installation.
func (g *GitHubApp) MintRepoToken(ctx context.Context, repo string, perms map[string]string) (string, error) {
	return g.mintToken(ctx, repo, perms)
}

func (g *GitHubApp) mintToken(ctx context.Context, repo string, perms map[string]string) (string, error) {
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
	// GitHub's create-installation-access-token narrows the token: "repositories" (short names) limits
	// it to those repos, "permissions" limits it to a subset of the App's grants. An empty body =
	// installation-wide (kept only for MintInstallationToken).
	body := map[string]any{}
	if repo != "" {
		name := repo
		if _, n, ok := strings.Cut(repo, "/"); ok {
			name = n
		}
		body["repositories"] = []string{name}
	}
	if len(perms) > 0 {
		body["permissions"] = perms
	}
	var reqBody any
	if len(body) > 0 {
		reqBody = body
	}
	var out struct {
		Token string `json:"token"`
	}
	url := "https://api.github.com/app/installations/" + instID + "/access_tokens"
	if err := g.apiJSONBody(ctx, http.MethodPost, url, jwt, reqBody, &out); err != nil {
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
// at-most-once, so a real PR must never be mistaken for "no PR".
func (g *GitHubApp) FindOpenPRByBranch(ctx context.Context, repo, branch string) (int, string, error) {
	token, err := g.MintRepoToken(ctx, repo, map[string]string{"pull_requests": "read"})
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

// ThreadComment is one human contribution to a PR conversation — an inline review comment, a
// top-level PR comment, or a submitted review — normalized across GitHub's three endpoints.
type ThreadComment struct {
	ID      int64  `json:"id"`
	Author  string `json:"author"`
	Body    string `json:"body"`
	Path    string `json:"path,omitempty"`  // inline review comments only
	Line    int    `json:"line,omitempty"`  // inline review comments only
	Kind    string `json:"kind"`            // review_comment | issue_comment | review
	State   string `json:"state,omitempty"` // reviews only (changes_requested|approved|commented)
	Created string `json:"created"`         // ISO-8601, for ordering
}

// FetchPRThread returns the PR's full conversation from GitHub — the source of truth — so the
// workflow can reconcile against dropped webhooks and never leave the agent missing a comment
// (at-most-once delivery). Bot-authored entries (the agent's own replies) are excluded so
// they never re-enter as feedback, and empty entries (a bare "commented" review) are dropped.
// Results are ordered oldest-first. One page (100) of each kind — ample for a review thread.
func (g *GitHubApp) FetchPRThread(ctx context.Context, repo string, prNumber int, botUser string) ([]ThreadComment, error) {
	token, err := g.MintRepoToken(ctx, repo, map[string]string{"pull_requests": "read"})
	if err != nil {
		return nil, err
	}
	isBot := func(login, typ string) bool {
		return typ == "Bot" || (botUser != "" && login == botUser)
	}

	var out []ThreadComment

	// Inline review comments.
	var review []struct {
		ID      int64                        `json:"id"`
		Body    string                       `json:"body"`
		Path    string                       `json:"path"`
		Line    int                          `json:"line"`
		Created string                       `json:"created_at"`
		User    struct{ Login, Type string } `json:"user"`
	}
	if err := g.getJSON(ctx, token, fmt.Sprintf(
		"https://api.github.com/repos/%s/pulls/%d/comments?per_page=100", repo, prNumber), &review); err != nil {
		return nil, err
	}
	for _, c := range review {
		if isBot(c.User.Login, c.User.Type) || strings.TrimSpace(c.Body) == "" {
			continue
		}
		out = append(out, ThreadComment{ID: c.ID, Author: c.User.Login, Body: c.Body,
			Path: c.Path, Line: c.Line, Kind: "review_comment", Created: c.Created})
	}

	// Top-level PR (issue) comments.
	var issue []struct {
		ID      int64                        `json:"id"`
		Body    string                       `json:"body"`
		Created string                       `json:"created_at"`
		User    struct{ Login, Type string } `json:"user"`
	}
	if err := g.getJSON(ctx, token, fmt.Sprintf(
		"https://api.github.com/repos/%s/issues/%d/comments?per_page=100", repo, prNumber), &issue); err != nil {
		return nil, err
	}
	for _, c := range issue {
		if isBot(c.User.Login, c.User.Type) || strings.TrimSpace(c.Body) == "" {
			continue
		}
		out = append(out, ThreadComment{ID: c.ID, Author: c.User.Login, Body: c.Body,
			Kind: "issue_comment", Created: c.Created})
	}

	// Submitted reviews (the summary body, not the inline comments those carry).
	var reviews []struct {
		ID      int64                        `json:"id"`
		Body    string                       `json:"body"`
		State   string                       `json:"state"`
		Created string                       `json:"submitted_at"`
		User    struct{ Login, Type string } `json:"user"`
	}
	if err := g.getJSON(ctx, token, fmt.Sprintf(
		"https://api.github.com/repos/%s/pulls/%d/reviews?per_page=100", repo, prNumber), &reviews); err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if isBot(r.User.Login, r.User.Type) || strings.TrimSpace(r.Body) == "" {
			continue
		}
		out = append(out, ThreadComment{ID: r.ID, Author: r.User.Login, Body: r.Body,
			Kind: "review", State: strings.ToLower(r.State), Created: r.Created})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
	return out, nil
}

// getJSON GETs an installation-authenticated GitHub endpoint and unmarshals the body.
func (g *GitHubApp) getJSON(ctx context.Context, token, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// FetchFile returns the raw bytes of path in repo at ref (the repo's default branch when ref is
// empty), or (nil, nil) if the file does not exist. Used to read a repo's .mandobox.yml.
func (g *GitHubApp) FetchFile(ctx context.Context, repo, path, ref string) ([]byte, error) {
	token, err := g.MintRepoToken(ctx, repo, map[string]string{"contents": "read"})
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s", repo, path)
	if ref != "" {
		url += "?ref=" + ref
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github.raw")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // no config file — caller falls back to defaults
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("fetch %s in %s: %s: %s", path, repo, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// PostPRComment posts a top-level comment on the pull request (a PR is an issue for the comments
// API) — used when the reply isn't in response to a specific inline review comment.
func (g *GitHubApp) PostPRComment(ctx context.Context, repo string, prNumber int, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repo, prNumber)
	return g.postComment(ctx, "post pr comment", repo, url, body)
}

// PostReviewCommentReply threads a reply under a specific inline review comment, so the agent's
// answer lands right where the reviewer asked.
func (g *GitHubApp) PostReviewCommentReply(ctx context.Context, repo string, prNumber int, commentID int64, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d/comments/%d/replies", repo, prNumber, commentID)
	return g.postComment(ctx, "reply to review comment", repo, url, body)
}

func (g *GitHubApp) postComment(ctx context.Context, what, repo, url, body string) error {
	token, err := g.MintRepoToken(ctx, repo, map[string]string{"pull_requests": "write"})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s: %s", what, resp.Status, strings.TrimSpace(string(rb)))
	}
	return nil
}

// apiJSONBody is apiJSON with an optional JSON request body — used to scope a minted token.
func (g *GitHubApp) apiJSONBody(ctx context.Context, method, url, jwt string, reqBody, out any) error {
	var rdr io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if rdr != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
