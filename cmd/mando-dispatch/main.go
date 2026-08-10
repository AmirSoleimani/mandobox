// Command mando-dispatch starts one PRWorkflow — the M4 replacement for scripts/dispatch-vm.sh.
// It generates a session_id (also the workflow ID, §5) and hands the task to Temporal, which
// mints credentials, launches the VM, and drives the review loop. Run it on the fleet host (or
// anywhere that can reach the Temporal frontend).
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chelodo/mandobox/internal/control"
	"github.com/chelodo/mandobox/internal/session"
	"go.temporal.io/sdk/client"
)

func main() {
	cloneURL := os.Getenv("REPO_CLONE_URL")
	imageSHA := os.Getenv("IMAGE_SHA")
	if cloneURL == "" || imageSHA == "" {
		log.Fatal("REPO_CLONE_URL and IMAGE_SHA are required")
	}
	sid, err := session.New()
	if err != nil {
		log.Fatalf("session id: %v", err)
	}

	in := control.WorkflowInput{
		SessionID:  sid.String(),
		Repo:       env("REPO_SLUG", ""),
		CloneURL:   cloneURL,
		BaseBranch: env("BASE_BRANCH", "main"),
		Prompt:     env("PROMPT", "Make a small, well-scoped improvement and open a PR."),
		ImageSHA:   imageSHA,
		// Left unset by default so the resolved config (box + repo .mandobox.yml) decides. Setting
		// these env vars makes them explicit per-task overrides (a concrete model id; LiteLLM routes it).
		Model:  env("CLAUDE_MODEL", ""),
		VCPUs:  atoi(env("VCPUS", "0")),
		MemMiB: atoi(env("MEM_MIB", "0")),
		// KEEP_ALIVE: how long a warm VM stays up while idle before it parks. A duration ("30m",
		// "2h"), or "never" to keep it warm for the PR's whole life (still capped by HardTTL).
		// Empty → the 15m default.
		Policy: control.Policy{KeepAlive: parseKeepAlive(os.Getenv("KEEP_ALIVE"))},
	}

	c, err := client.Dial(client.Options{
		HostPort:  env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "fleet"),
	})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer c.Close()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        sid.String(), // workflow ID == session ID
		TaskQueue: control.TaskQueue,
	}, control.PRWorkflow, in)
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}
	log.Printf("dispatched session=%s workflow=%s run=%s branch=agent/%s",
		sid, run.GetID(), run.GetRunID(), sid)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// parseKeepAlive turns the KEEP_ALIVE env into a Policy.KeepAlive: a duration, the "never park"
// sentinel (-1) for keep-warm-for-the-PR's-life, or 0 (unset) so the workflow applies its default.
func parseKeepAlive(s string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return 0 // → 15m default in the workflow
	case "never", "off", "none", "forever":
		return -1 // never park while the PR is open (bounded by HardTTL)
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		log.Printf("KEEP_ALIVE %q is not a duration (e.g. 30m, 2h) or 'never'; using default", s)
		return 0
	}
	return d
}
