package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// instructionsStore manages the box-wide default agent instructions — the system-prompt addition
// every session inherits unless its repo's .mandobox.yml sets its own. It's a plain-text file (no
// YAML), so editing is a simple round-trip. The worker's ResolveConfig re-reads it per dispatch, so
// a save takes effect on the next task with no restart.
type instructionsStore struct {
	path string
}

func newInstructionsStore(path string) *instructionsStore { return &instructionsStore{path: path} }

type instructionsView struct {
	Path     string `json:"path"`
	Raw      string `json:"raw"`
	Exists   bool   `json:"exists"`
	Modified string `json:"modified,omitempty"`
}

func (s *instructionsStore) read() (instructionsView, error) {
	v := instructionsView{Path: s.path}
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return v, nil // absent → no box-wide instructions (built-in behavior)
		}
		return v, fmt.Errorf("read %s: %w", s.path, err)
	}
	v.Exists = true
	v.Raw = string(b)
	if fi, err := os.Stat(s.path); err == nil {
		v.Modified = fi.ModTime().UTC().Format(time.RFC3339)
	}
	return v, nil
}

// write atomically replaces the instructions file, keeping a single .bak. An empty body removes the
// box-wide instructions (the file is truncated to empty, which the worker treats as "none").
func (s *instructionsStore) write(raw string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir instructions dir: %w", err)
	}
	if cur, err := os.ReadFile(s.path); err == nil {
		_ = os.WriteFile(s.path+".bak", cur, 0o644)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		return fmt.Errorf("write temp instructions: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace instructions: %w", err)
	}
	return nil
}
