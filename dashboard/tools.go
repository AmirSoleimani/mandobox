package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// toolStore surfaces the pinned agent-CLI versions (tools.env), the active image sha, and the update
// audit trail, and drives update-tools.sh. An image assemble takes ~40s, so updates run as a single
// tracked background job the UI polls — never two at once, and the box's other work is untouched.
type toolStore struct {
	toolsEnv    string // /usr/local/lib/fleet/tools.env
	updateTools string // /usr/local/lib/fleet/update-tools.sh
	auditLog    string // /var/lib/fleet/tool-updates.log
	imagesDir   string // /var/lib/fleet/images (holds current.sha)

	mu  sync.Mutex
	job *updateJob
}

func newToolStore(toolsEnv, updateTools, auditLog, imagesDir string) *toolStore {
	return &toolStore{toolsEnv: toolsEnv, updateTools: updateTools, auditLog: auditLog, imagesDir: imagesDir}
}

type toolsView struct {
	Claude     string       `json:"claude_version"`
	Codex      string       `json:"codex_version"`
	CurrentSHA string       `json:"current_sha"`
	Audit      []auditEntry `json:"audit"`
	Job        *jobView     `json:"job,omitempty"`
}

type auditEntry struct {
	Timestamp string `json:"timestamp"`
	SHA       string `json:"sha"`
	Claude    string `json:"claude"`
	Codex     string `json:"codex"`
}

func (t *toolStore) view() (toolsView, error) {
	v := toolsView{}
	env := parseEnvFile(t.toolsEnv)
	v.Claude = env["CLAUDE_CODE_VERSION"]
	v.Codex = env["CODEX_VERSION"]
	if b, err := os.ReadFile(t.imagesDir + "/current.sha"); err == nil {
		v.CurrentSHA = strings.TrimSpace(string(b))
	}
	v.Audit = t.readAudit(20)
	t.mu.Lock()
	if t.job != nil {
		jv := t.job.view()
		v.Job = &jv
	}
	t.mu.Unlock()
	return v, nil
}

// parseEnvFile reads KEY=VALUE lines (shell env format), ignoring comments and blanks.
func parseEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out
}

// readAudit returns the most recent n update records, newest first. Each line is written by
// update-tools.sh as: <ts>\tsha=<sha>\tclaude-code=<v>\tcodex=<v>.
func (t *toolStore) readAudit(n int) []auditEntry {
	f, err := os.Open(t.auditLog)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if s := strings.TrimSpace(sc.Text()); s != "" {
			lines = append(lines, s)
		}
	}
	var out []auditEntry
	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, parseAuditLine(lines[i]))
	}
	return out
}

func parseAuditLine(line string) auditEntry {
	e := auditEntry{}
	for f := range strings.SplitSeq(line, "\t") {
		switch {
		case strings.HasPrefix(f, "sha="):
			e.SHA = strings.TrimPrefix(f, "sha=")
		case strings.HasPrefix(f, "claude-code="):
			e.Claude = strings.TrimPrefix(f, "claude-code=")
		case strings.HasPrefix(f, "codex="):
			e.Codex = strings.TrimPrefix(f, "codex=")
		default:
			if e.Timestamp == "" {
				e.Timestamp = f
			}
		}
	}
	return e
}

// ---- update job ----------------------------------------------------------

type updateJob struct {
	mu       sync.Mutex
	started  time.Time
	claude   string
	codex    string
	running  bool
	ok       bool
	err      string
	output   bytes.Buffer
	finished time.Time
}

type jobView struct {
	Running  bool   `json:"running"`
	OK       bool   `json:"ok"`
	Claude   string `json:"claude,omitempty"`
	Codex    string `json:"codex,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Error    string `json:"error,omitempty"`
	Output   string `json:"output,omitempty"`
}

func (j *updateJob) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{Running: j.running, OK: j.ok, Claude: j.claude, Codex: j.codex, Error: j.err, Output: j.output.String()}
	if !j.started.IsZero() {
		v.Started = j.started.UTC().Format(time.RFC3339)
	}
	if !j.finished.IsZero() {
		v.Finished = j.finished.UTC().Format(time.RFC3339)
	}
	return v
}

// startUpdate kicks off update-tools.sh in the background if no update is already running. It
// returns the job view immediately; the UI polls GET /api/tools to watch it complete. At most one
// update runs at a time so we never activate two images concurrently.
func (t *toolStore) startUpdate(claude, codex string) (jobView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job != nil && t.job.isRunning() {
		return t.job.view(), fmt.Errorf("an update is already running")
	}
	if _, err := os.Stat(t.updateTools); err != nil {
		return jobView{}, fmt.Errorf("update-tools.sh not found at %s", t.updateTools)
	}
	job := &updateJob{started: nowFn(), claude: claude, codex: codex, running: true}
	t.job = job
	go t.runUpdate(job, claude, codex)
	return job.view(), nil
}

func (j *updateJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// runUpdate executes update-tools.sh with an overall timeout well above the ~40s assemble, streaming
// combined output into the job buffer for the UI.
func (t *toolStore) runUpdate(job *updateJob, claude, codex string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	args := []string{}
	if claude != "" {
		args = append(args, "--claude", claude)
	}
	if codex != "" {
		args = append(args, "--codex", codex)
	}
	cmd := exec.CommandContext(ctx, t.updateTools, args...)
	cmd.Stdout = &jobWriter{job: job}
	cmd.Stderr = &jobWriter{job: job}
	err := cmd.Run()

	job.mu.Lock()
	job.running = false
	job.finished = nowFn()
	if err != nil {
		job.ok = false
		job.err = err.Error()
	} else {
		job.ok = true
	}
	job.mu.Unlock()
}

// jobWriter appends process output to the job buffer under its lock so the polling view is safe to
// read mid-run.
type jobWriter struct{ job *updateJob }

func (w *jobWriter) Write(p []byte) (int, error) {
	w.job.mu.Lock()
	w.job.output.Write(p)
	w.job.mu.Unlock()
	return len(p), nil
}

// nowFn is a package var so tests can stamp deterministic times; production uses the real clock.
var nowFn = time.Now
