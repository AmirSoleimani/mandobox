// Command fleet-dispatch starts one PRWorkflow — the M4 replacement for scripts/dispatch-vm.sh.
// It generates a session_id (also the workflow ID, §5) and hands the task to Temporal, which
// mints credentials, launches the VM, and drives the review loop. Run it on the fleet host (or
// anywhere that can reach the Temporal frontend).
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/chelodo/fleet/internal/control"
	"github.com/chelodo/fleet/internal/session"
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
		// Real model ID — Claude Code expands its own aliases, so pass a concrete id; LiteLLM
		// routes it (§10). Use a cheaper id (e.g. claude-haiku-4-5-20251001) for cheap work.
		Model: env("CLAUDE_MODEL", "claude-sonnet-5"),
		VCPUs:      atoi(env("VCPUS", "2")),
		MemMiB:     atoi(env("MEM_MIB", "4096")),
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
