// Command nats-bridge subscribes to the guest NATS streams and persists them to durable
// per-session storage (PLAN §6.3: log lines go to storage, not through workflow history).
// The workflow learns terminal outcomes via RunAgentPhase; this process is the archival +
// observability path. Slack forwarding is layered on in M5.
package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/nats-io/nats.go"
)

func main() {
	url := env("NATS_URL", "nats://172.31.0.1:4222")
	logDir := env("FLEET_LOG_DIR", "/var/lib/fleet/logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		log.Fatalf("log dir: %v", err)
	}

	nc, err := nats.Connect(url, nats.Name("nats-bridge"), nats.MaxReconnects(-1))
	if err != nil {
		log.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	w := &writers{dir: logDir, files: map[string]*os.File{}}
	defer w.closeAll()

	for _, kind := range []string{"event", "log", "heartbeat"} {
		k := kind
		if _, err := nc.Subscribe("agent.*."+k, func(m *nats.Msg) { w.handle(m.Subject, k, m.Data) }); err != nil {
			log.Fatalf("subscribe %s: %v", k, err)
		}
	}
	_ = nc.Flush()
	log.Printf("nats-bridge: archiving agent.*.{event,log,heartbeat} to %s from %s", logDir, url)

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

// writers holds one append file per (session, kind), opened lazily.
type writers struct {
	dir   string
	mu    sync.Mutex
	files map[string]*os.File
}

func (w *writers) handle(subject, kind string, data []byte) {
	sid := sessionFromSubject(subject)
	if sid == "" {
		return
	}
	if kind == "heartbeat" {
		return // liveness only; not archived
	}
	f, err := w.fileFor(sid, kind)
	if err != nil {
		log.Printf("open %s/%s: %v", sid, kind, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("write %s/%s: %v", sid, kind, err)
	}
	if kind == "event" {
		log.Printf("event %s: %s", sid, strings.TrimSpace(string(data)))
	}
}

func (w *writers) fileFor(sid, kind string) (*os.File, error) {
	key := sid + "." + kind
	w.mu.Lock()
	defer w.mu.Unlock()
	if f, ok := w.files[key]; ok {
		return f, nil
	}
	path := filepath.Join(w.dir, sid+"."+kind+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	w.files[key] = f
	return f, nil
}

func (w *writers) closeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, f := range w.files {
		_ = f.Close()
	}
}

// sessionFromSubject parses "agent.<sid>.<kind>" → "<sid>".
func sessionFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 || parts[0] != "agent" {
		return ""
	}
	return parts[1]
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
