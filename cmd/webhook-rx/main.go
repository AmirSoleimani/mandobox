// Command webhook-rx receives GitHub webhooks and translates PR-lifecycle events into Temporal
// signals (PLAN §6.2). It verifies the HMAC signature, extracts the repo + PR number, finds the
// owning PRWorkflow by search attributes, and signals it. Dedupe/debounce live in the workflow;
// this process is deliberately dumb — it translates, it does not decide.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/acme/fleet/internal/control"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

type server struct {
	c         client.Client
	namespace string
	secret    []byte
}

func main() {
	addr := env("WEBHOOK_ADDR", ":8088")
	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Fatal("GITHUB_WEBHOOK_SECRET is required")
	}
	c, err := client.Dial(client.Options{
		HostPort:  env("TEMPORAL_ADDRESS", "127.0.0.1:7233"),
		Namespace: env("TEMPORAL_NAMESPACE", "fleet"),
	})
	if err != nil {
		log.Fatalf("temporal dial: %v", err)
	}
	defer c.Close()

	s := &server{c: c, namespace: env("TEMPORAL_NAMESPACE", "fleet"), secret: []byte(secret)}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/webhook", s.handle)

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("webhook-rx: listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	if !s.verify(r.Header.Get("X-Hub-Signature-256"), body) {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	delivery := r.Header.Get("X-GitHub-Delivery")

	sig, repo, prNumber, ok := mapEvent(event, delivery, body)
	if !ok {
		w.WriteHeader(http.StatusAccepted) // event we don't route (e.g. ping) — accept and ignore
		return
	}
	wfID, err := s.findWorkflow(r.Context(), repo, prNumber)
	if err != nil {
		// No live workflow for this PR — not our concern. Accept so GitHub stops retrying.
		log.Printf("no workflow for %s#%d (%s): %v", repo, prNumber, event, err)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.c.SignalWorkflow(r.Context(), wfID, "", sig.name, sig.payload); err != nil {
		log.Printf("signal %s %s: %v", wfID, sig.name, err)
		http.Error(w, "signal", http.StatusInternalServerError)
		return
	}
	log.Printf("signaled %s -> %s (%s#%d, delivery %s)", wfID, sig.name, repo, prNumber, delivery)
	w.WriteHeader(http.StatusOK)
}

func (s *server) verify(header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// findWorkflow locates the Running PRWorkflow for repo+PR via the visibility search attributes
// the workflow upserts (repo, pr_number). Returns the workflow ID.
func (s *server) findWorkflow(ctx context.Context, repo string, prNumber int) (string, error) {
	query := fmt.Sprintf(`WorkflowType='PRWorkflow' AND %s='%s' AND %s=%d AND ExecutionStatus='Running'`,
		control.SARepo, repo, control.SAPRNumber, prNumber)
	resp, err := s.c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace: s.namespace, Query: query, PageSize: 1,
	})
	if err != nil {
		return "", err
	}
	if len(resp.Executions) == 0 {
		return "", fmt.Errorf("not found")
	}
	return resp.Executions[0].Execution.WorkflowId, nil
}

type signal struct {
	name    string
	payload any
}

// mapEvent extracts (signal, repo, prNumber) from a GitHub webhook, or ok=false to ignore it.
func mapEvent(event, delivery string, body []byte) (signal, string, int, bool) {
	var p githubPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return signal{}, "", 0, false
	}
	repo := p.Repository.FullName
	switch event {
	case "pull_request_review_comment":
		return signal{control.SignalReviewComment, control.ReviewCommentSignal{
			Body: p.Comment.Body, Author: p.Comment.User.Login,
			Path: p.Comment.Path, Line: p.Comment.Line, DeliveryID: delivery,
		}}, repo, p.PullRequest.Number, true
	case "issue_comment":
		if p.Issue.PullRequest == nil { // only PR comments matter
			return signal{}, "", 0, false
		}
		return signal{control.SignalReviewComment, control.ReviewCommentSignal{
			Body: p.Comment.Body, Author: p.Comment.User.Login, DeliveryID: delivery,
		}}, repo, p.Issue.Number, true
	case "pull_request_review":
		return signal{control.SignalReviewSubmitted, control.ReviewSubmittedSignal{
			State: strings.ToLower(p.Review.State), Body: p.Review.Body,
			Author: p.Review.User.Login, DeliveryID: delivery,
		}}, repo, p.PullRequest.Number, true
	case "pull_request":
		if p.Action != "closed" {
			return signal{}, "", 0, false
		}
		return signal{control.SignalPRClosed, control.PRClosedSignal{
			Merged: p.PullRequest.Merged, DeliveryID: delivery,
		}}, repo, p.PullRequest.Number, true
	case "check_suite":
		if p.Action != "completed" || len(p.CheckSuite.PullRequests) == 0 {
			return signal{}, "", 0, false
		}
		return signal{control.SignalCIStatus, control.CIStatusSignal{
			Conclusion: p.CheckSuite.Conclusion, DeliveryID: delivery,
		}}, repo, p.CheckSuite.PullRequests[0].Number, true
	default:
		return signal{}, "", 0, false
	}
}

// githubPayload is the union of the webhook fields we read across event types.
type githubPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	PullRequest struct {
		Number int  `json:"number"`
		Merged bool `json:"merged"`
	} `json:"pull_request"`
	Issue struct {
		Number      int             `json:"number"`
		PullRequest json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		Body string `json:"body"`
		Path string `json:"path"` // inline review comments carry the file + line
		Line int    `json:"line"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	Review struct {
		State string `json:"state"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"review"`
	CheckSuite struct {
		Conclusion   string `json:"conclusion"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	} `json:"check_suite"`
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
