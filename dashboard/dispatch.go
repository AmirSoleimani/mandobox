package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// dispatch.go starts a new session by shelling out to the canonical mando-dispatch (the same path
// the slack-gateway uses), so the dashboard doesn't reinvent session-id/clone-url/input construction.
// It's env-driven; exec.Command with an explicit Env (no shell) means the form values can't inject.

var repoSlugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
var dispatchSessionRe = regexp.MustCompile(`session=(\S+)`)

type dispatchStore struct {
	bin          string // /usr/local/bin/mando-dispatch
	imagesDir    string // holds current.sha
	temporalAddr string
	namespace    string
}

func newDispatchStore(bin, imagesDir, temporalAddr, namespace string) *dispatchStore {
	return &dispatchStore{bin: bin, imagesDir: imagesDir, temporalAddr: temporalAddr, namespace: namespace}
}

type dispatchRequest struct {
	Repo       string `json:"repo"`
	Prompt     string `json:"prompt"`
	BaseBranch string `json:"base_branch"`
	Model      string `json:"model"`
	KeepAlive  string `json:"keep_alive"`
	VCPUs      int    `json:"vcpus"`
	MemMiB     int    `json:"mem_mib"`
}

// dispatch validates the request, resolves the active image, and runs mando-dispatch. It returns the
// new session id (parsed from mando-dispatch's output) so the UI can jump straight to its console.
func (d *dispatchStore) dispatch(ctx context.Context, req dispatchRequest) (string, error) {
	repo := strings.TrimSpace(req.Repo)
	if !repoSlugRe.MatchString(repo) {
		return "", fmt.Errorf("repo must be owner/name")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return "", fmt.Errorf("task is empty")
	}
	if _, err := os.Stat(d.bin); err != nil {
		return "", fmt.Errorf("mando-dispatch not found at %s", d.bin)
	}
	sha := ""
	if b, err := os.ReadFile(d.imagesDir + "/current.sha"); err == nil {
		sha = strings.TrimSpace(string(b))
	}
	if sha == "" {
		return "", fmt.Errorf("no active golden image (current.sha) — build one on the Tools tab first")
	}
	base := strings.TrimSpace(req.BaseBranch)
	if base == "" {
		base = "main"
	}

	env := append(os.Environ(),
		"REPO_SLUG="+repo,
		"REPO_CLONE_URL=https://github.com/"+repo+".git",
		"IMAGE_SHA="+sha,
		"BASE_BRANCH="+base,
		"PROMPT="+req.Prompt,
		"TEMPORAL_ADDRESS="+d.temporalAddr,
		"TEMPORAL_NAMESPACE="+d.namespace,
	)
	if req.Model != "" {
		env = append(env, "CLAUDE_MODEL="+req.Model)
	}
	if req.KeepAlive != "" {
		env = append(env, "KEEP_ALIVE="+req.KeepAlive)
	}
	if req.VCPUs > 0 {
		env = append(env, "VCPUS="+strconv.Itoa(req.VCPUs))
	}
	if req.MemMiB > 0 {
		env = append(env, "MEM_MIB="+strconv.Itoa(req.MemMiB))
	}

	cmd := exec.CommandContext(ctx, d.bin)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("dispatch failed: %s", strings.TrimSpace(lastLine(string(out))))
	}
	if m := dispatchSessionRe.FindStringSubmatch(string(out)); len(m) == 2 {
		return m[1], nil
	}
	return "", nil // dispatched, but couldn't parse the session id
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
