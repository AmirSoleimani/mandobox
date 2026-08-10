package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// activity.go streams a session's live agent activity to the browser. The guest supervisor runs
// Claude with --output-format stream-json; nats-bridge archives every line to
// <log-dir>/<session>.log.jsonl. We tail that file and parse each stream-json line into a compact,
// human-readable event, pushed over Server-Sent Events (one-way, tunnel-friendly, auto-reconnecting).
// It's read-only — a glance at what the agent is doing, complementary to the heavier VS Code attach.

// sessionIDRe guards the path against traversal — only a bare session id maps to a log file.
var sessionIDRe = regexp.MustCompile(`^s_[A-Za-z0-9]+$`)

type activityStore struct {
	logDir string
}

func newActivityStore(logDir string) *activityStore { return &activityStore{logDir: logDir} }

func (a *activityStore) logPath(id string) (string, bool) {
	if !sessionIDRe.MatchString(id) {
		return "", false
	}
	return filepath.Join(a.logDir, id+".log.jsonl"), true
}

// activityEvent is the compact shape the browser renders.
type activityEvent struct {
	Seq  int    `json:"seq"`
	Time string `json:"time,omitempty"`
	Kind string `json:"kind"` // meta|system|thinking|text|tool|tool_result|result
	Tool string `json:"tool,omitempty"`
	Text string `json:"text,omitempty"`
}

// stream does scrollback + follow: it emits the file's existing lines, then tails it for new ones,
// until the client disconnects (ctx cancelled). Reopening each tick keeps it simple and survives the
// file not existing yet (session just dispatched) or being (re)created.
func (a *activityStore) stream(ctx context.Context, w io.Writer, flush func(), path string) {
	send := func(ev activityEvent) {
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flush()
	}
	comment := func(s string) { fmt.Fprintf(w, ": %s\n\n", s); flush() }

	fmt.Fprint(w, "retry: 3000\n\n")
	flush()

	var seq int
	var offset int64
	var partial []byte
	seen := false
	ticks := 0
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ticks++

		f, err := os.Open(path)
		if err != nil {
			if ticks%10 == 0 { // ~5s
				comment("waiting for activity")
			}
			continue
		}
		if !seen {
			seen = true
			send(activityEvent{Seq: seq, Kind: "meta", Text: "connected — streaming agent activity"})
			seq++
		}
		if _, err := f.Seek(offset, io.SeekStart); err == nil {
			data, _ := io.ReadAll(io.LimitReader(f, 8<<20)) // bound a single read
			offset += int64(len(data))
			partial = append(partial, data...)
			for {
				i := bytes.IndexByte(partial, '\n')
				if i < 0 {
					break
				}
				line := partial[:i]
				partial = partial[i+1:]
				for _, ev := range parseStreamLine(line, &seq) {
					send(ev)
				}
			}
		}
		_ = f.Close()
		if ticks%20 == 0 { // ~10s keepalive so proxies/tunnels don't drop an idle stream
			comment("ping")
		}
	}
}

// parseStreamLine turns one Claude stream-json line into zero or more activity events. An assistant
// message can carry several content blocks (thinking + tool_use), each becoming its own event.
func parseStreamLine(raw []byte, seq *int) []activityEvent {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var o struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		Timestamp string          `json:"timestamp"`
		Model     string          `json:"model"`
		Message   json.RawMessage `json:"message"`
		Result    string          `json:"result"`
		Cost      float64         `json:"total_cost_usd"`
		Duration  int             `json:"duration_ms"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil
	}
	next := func(kind, tool, text string) activityEvent {
		ev := activityEvent{Seq: *seq, Time: o.Timestamp, Kind: kind, Tool: tool, Text: text}
		*seq++
		return ev
	}

	switch o.Type {
	case "system":
		if o.Subtype == "init" || o.Subtype == "" {
			t := "session started"
			if o.Model != "" {
				t += " · " + o.Model
			}
			return []activityEvent{next("system", "", t)}
		}
	case "assistant", "user":
		return parseMessage(o.Message, next)
	case "result":
		t := "turn complete"
		if o.Cost > 0 || o.Duration > 0 {
			t = fmt.Sprintf("turn complete · $%.4f · %ds", o.Cost, o.Duration/1000)
		}
		return []activityEvent{next("result", "", t)}
	}
	return nil
}

func parseMessage(rawMsg json.RawMessage, next func(kind, tool, text string) activityEvent) []activityEvent {
	var msg struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"` // tool_result content
		} `json:"content"`
	}
	if err := json.Unmarshal(rawMsg, &msg); err != nil {
		return nil
	}
	var out []activityEvent
	for _, c := range msg.Content {
		switch c.Type {
		case "thinking":
			if s := clip(c.Thinking, 600); s != "" {
				out = append(out, next("thinking", "", s))
			}
		case "text":
			if s := clip(c.Text, 4000); s != "" {
				out = append(out, next("text", "", s))
			}
		case "tool_use":
			out = append(out, next("tool", c.Name, summarizeToolInput(c.Name, c.Input)))
		case "tool_result":
			if s := clip(firstLine(toolResultText(c.Content)), 200); s != "" {
				out = append(out, next("tool_result", "", s))
			}
		}
	}
	return out
}

// summarizeToolInput picks the most informative field of a tool call so the feed reads like
// "Read main.go" / "Bash: go test ./..." rather than dumping raw JSON.
func summarizeToolInput(name string, input json.RawMessage) string {
	var m map[string]any
	_ = json.Unmarshal(input, &m)
	get := func(k string) string { s, _ := m[k].(string); return s }
	switch name {
	case "Bash":
		return clip(get("command"), 300)
	case "Read", "Edit", "Write", "NotebookEdit":
		return clip(get("file_path"), 300)
	case "Grep":
		if p := get("path"); p != "" {
			return clip(get("pattern")+" in "+p, 300)
		}
		return clip(get("pattern"), 300)
	case "Glob":
		return clip(get("pattern"), 300)
	case "Task", "Agent":
		return clip(firstNonEmpty(get("description"), get("prompt")), 300)
	case "WebFetch", "WebSearch":
		return clip(firstNonEmpty(get("url"), get("query")), 300)
	default:
		// Fall back to the first short string value, if any.
		for _, k := range []string{"description", "path", "url", "query", "pattern", "command", "file_path"} {
			if v := get(k); v != "" {
				return clip(v, 300)
			}
		}
		return ""
	}
}

// toolResultText extracts text from a tool_result's content, which is either a string or an array of
// {type:"text", text:...} blocks.
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, x := range blocks {
			if x.Text != "" {
				b.WriteString(x.Text)
				b.WriteByte(' ')
			}
		}
		return b.String()
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// sseHeaders configures a response for Server-Sent Events.
func sseHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}
