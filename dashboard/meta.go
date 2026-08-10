package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// meta.go reads the durable per-session metadata the worker writes at launch (<session>.meta.json in
// the log dir): the model, provider, and auth the agent actually ran on. Like the cost archive it
// outlives the workflow, so the Sessions list and Costs can show "how it ran" for a closed session —
// invaluable for later reference and analysis.

type metaStore struct{ dir string }

func newMetaStore(dir string) *metaStore { return &metaStore{dir: dir} }

type sessionMetaRecord struct {
	SessionID    string `json:"session_id"`
	Repo         string `json:"repo,omitempty"`
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"` // claude_api | claude_subscription | codex
	Subscription bool   `json:"subscription,omitempty"`
	Harness      string `json:"harness,omitempty"` // claude | codex
	ImageSHA     string `json:"image_sha,omitempty"`
	Started      string `json:"started,omitempty"`
}

// all returns every session's meta, keyed by session id. Missing/corrupt files are skipped.
func (m *metaStore) all() map[string]sessionMetaRecord {
	out := map[string]sessionMetaRecord{}
	files, _ := filepath.Glob(filepath.Join(m.dir, "*.meta.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r sessionMetaRecord
		if json.Unmarshal(b, &r) != nil {
			continue
		}
		sid := r.SessionID
		if sid == "" {
			sid = strings.TrimSuffix(filepath.Base(f), ".meta.json")
		}
		out[sid] = r
	}
	return out
}
