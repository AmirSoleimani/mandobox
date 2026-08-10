// Command nats-bridge subscribes to the guest NATS streams and persists them to durable
// per-session storage (log lines go to storage, not through workflow history).
// The workflow learns terminal outcomes via RunAgentPhase; this process is the archival +
// observability path. Slack forwarding is layered on later.
package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/acme/mandobox/internal/session"
	"github.com/nats-io/nats.go"
)

// Archive caps bound guest-controlled writes to the shared host disk (the NATS bus is host-local, so
// a guest could otherwise loop-publish log lines and fill /var/lib/fleet — the same volume as
// Temporal/Postgres). Past a cap, further lines for that session/kind are dropped (logged once).
const (
	maxSessionBytes = 256 << 20 // 256 MiB per (session,kind) file
	maxTotalBytes   = 8 << 30   // 8 GiB across all archives this process
)

func main() {
	url := env("NATS_URL", "nats://172.31.0.1:4222")
	logDir := env("FLEET_LOG_DIR", "/var/lib/fleet/logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		log.Fatalf("log dir: %v", err)
	}

	// Once NATS auth is provisioned, connect with the broad service creds (agent.>). An absent file →
	// legacy unauthenticated connect, so this is safe to deploy before the server cutover.
	opts := []nats.Option{nats.Name("nats-bridge"), nats.MaxReconnects(-1)}
	if creds := env("MANDO_NATS_SERVICE_CREDS", "/etc/fleet/nats-service.creds"); creds != "" {
		if _, err := os.Stat(creds); err == nil {
			opts = append(opts, nats.UserCredentials(creds))
		}
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		log.Fatalf("nats connect: %v", err)
	}
	defer nc.Close()

	w := &writers{dir: logDir, files: map[string]*os.File{}, size: map[string]int64{}, dropped: map[string]bool{}}
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

// writers holds one append file per (session, kind), opened lazily, with per-file and aggregate byte
// caps so a guest can't fill the host disk through the archive path.
type writers struct {
	dir     string
	mu      sync.Mutex
	files   map[string]*os.File
	size    map[string]int64 // bytes written per key (seeded from the file on open)
	dropped map[string]bool  // key → cap reached (log-once)
	total   int64            // aggregate bytes across all archives
}

func (w *writers) handle(subject, kind string, data []byte) {
	sid := sessionFromSubject(subject)
	// The subject is guest-controlled; only a well-formed session id may become a filesystem path
	// (session.Pattern has no "." or "/", so it can't traverse). Heartbeats are liveness only.
	if sid == "" || !session.Pattern.MatchString(sid) || kind == "heartbeat" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	f, key, err := w.fileForLocked(sid, kind)
	if err != nil {
		log.Printf("open %s/%s: %v", sid, kind, err)
		return
	}
	n := int64(len(data)) + 1
	if w.size[key]+n > maxSessionBytes || w.total+n > maxTotalBytes {
		if !w.dropped[key] {
			w.dropped[key] = true
			log.Printf("nats-bridge: archive cap reached for %s/%s — dropping further lines", sid, kind)
		}
		return
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		log.Printf("write %s/%s: %v", sid, kind, err)
		return
	}
	w.size[key] += n
	w.total += n
	if kind == "event" {
		log.Printf("event %s: %s", sid, strings.TrimSpace(string(data)))
	}
}

// fileForLocked returns the append file for (sid,kind); the caller must hold w.mu.
func (w *writers) fileForLocked(sid, kind string) (*os.File, string, error) {
	key := sid + "." + kind
	if f, ok := w.files[key]; ok {
		return f, key, nil
	}
	path := filepath.Join(w.dir, sid+"."+kind+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, key, err
	}
	w.files[key] = f
	if fi, err := f.Stat(); err == nil { // seed the counter from any existing file (survives restart)
		w.size[key] = fi.Size()
		w.total += fi.Size()
	}
	return f, key, nil
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
