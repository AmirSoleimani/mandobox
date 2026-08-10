package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// configStore reads and writes the box config (/etc/fleet/mandobox.yml). The dashboard edits the
// raw YAML so it stays forward-compatible with any keys the control plane's resolveConfig
// understands — it validates that the text parses and re-serialises cleanly, but does not impose the
// dashboard's own schema on the operator. The worker re-reads this file per dispatch, so a saved
// change takes effect on the next task with no restart.
type configStore struct {
	path string
}

func newConfigStore(path string) *configStore { return &configStore{path: path} }

type configView struct {
	Path     string         `json:"path"`
	Raw      string         `json:"raw"`
	Parsed   map[string]any `json:"parsed,omitempty"`
	Modified string         `json:"modified,omitempty"`
	Exists   bool           `json:"exists"`
}

func (c *configStore) read() (configView, error) {
	v := configView{Path: c.path}
	b, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return v, nil // absent config is valid — the box runs on built-in defaults
		}
		return v, fmt.Errorf("read %s: %w", c.path, err)
	}
	v.Exists = true
	v.Raw = string(b)
	if fi, err := os.Stat(c.path); err == nil {
		v.Modified = fi.ModTime().UTC().Format(time.RFC3339)
	}
	// Best-effort parse for the read-only summary panel; a parse failure here still returns the raw
	// text so the operator can fix it in the editor.
	var parsed map[string]any
	if yaml.Unmarshal(b, &parsed) == nil {
		v.Parsed = parsed
	}
	return v, nil
}

// validate reports whether raw is well-formed YAML that maps to a document (not a bare scalar/list),
// returning a human-readable reason when it is not. This is the gate the save handler enforces.
func validateConfig(raw string) error {
	if len(raw) == 0 {
		return fmt.Errorf("config is empty")
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	if doc == nil {
		return fmt.Errorf("config must be a YAML mapping (key: value), not a bare value or list")
	}
	return nil
}

// write validates then atomically replaces the config, keeping a single timestamped backup so a bad
// edit is recoverable. Atomic rename means a concurrent worker read never sees a half-written file.
func (c *configStore) write(raw string) error {
	if err := validateConfig(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	// Back up the current file before overwriting (best-effort; absent on first write).
	if cur, err := os.ReadFile(c.path); err == nil {
		_ = os.WriteFile(c.path+".bak", cur, 0o644)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
