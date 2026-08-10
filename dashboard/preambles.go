package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// preambles.go manages the operator overrides of the agent's built-in task preambles — the base
// system prompts. Each preamble has an override file (edited here, read by the worker per launch)
// and a sibling ".default" file the worker materializes from the built-in constant, which the
// dashboard shows as the editable baseline and reset target. An absent/empty override → the guest
// uses its built-in default.
//
// Some lines in these preambles are load-bearing (e.g. "do NOT run git/gh" — the supervisor does the
// commit/push/PR; dropping it can silently break pushing), which the UI warns about.

type preambleDef struct {
	Name  string // "autonomous" | "collaborate"
	Label string
	Desc  string
	Path  string // override file; the built-in default is at Path + ".default"
}

type preambleStore struct {
	defs []preambleDef
}

func newPreambleStore(autonomousPath, collaboratePath string) *preambleStore {
	return &preambleStore{defs: []preambleDef{
		{
			Name: "autonomous", Label: "Autonomous preamble", Path: autonomousPath,
			Desc: "First/headless turn: the agent works without a human, makes assumptions, and completes the task by editing files. The supervisor handles commit/push/PR.",
		},
		{
			Name: "collaborate", Label: "Collaborate preamble", Path: collaboratePath,
			Desc: "Resume/review turn: the agent replies to the reviewer as a senior peer, answering questions and making changes, pushing back when warranted.",
		},
	}}
}

type preambleView struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Desc        string `json:"desc"`
	Path        string `json:"path"`
	Override    string `json:"override"`     // current override text ("" when none)
	HasOverride bool   `json:"has_override"` // true when a non-empty override file exists
	Default     string `json:"default"`      // built-in default (from the .default file)
	Modified    string `json:"modified,omitempty"`
}

func (s *preambleStore) view() []preambleView {
	out := make([]preambleView, 0, len(s.defs))
	for _, d := range s.defs {
		v := preambleView{Name: d.Name, Label: d.Label, Desc: d.Desc, Path: d.Path}
		if b, err := os.ReadFile(d.Path); err == nil {
			v.Override = string(b)
			v.HasOverride = strings.TrimSpace(v.Override) != ""
			if fi, err := os.Stat(d.Path); err == nil {
				v.Modified = fi.ModTime().UTC().Format(time.RFC3339)
			}
		}
		if b, err := os.ReadFile(d.Path + ".default"); err == nil {
			v.Default = string(b)
		}
		out = append(out, v)
	}
	return out
}

func (s *preambleStore) def(name string) (preambleDef, bool) {
	for _, d := range s.defs {
		if d.Name == name {
			return d, true
		}
	}
	return preambleDef{}, false
}

// write replaces a preamble override atomically (keeping one .bak). An empty body clears the override
// so the guest reverts to the built-in default on the next launch.
func (s *preambleStore) write(name, raw string) error {
	d, ok := s.def(name)
	if !ok {
		return fmt.Errorf("unknown preamble %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(d.Path), 0o755); err != nil {
		return fmt.Errorf("mkdir preamble dir: %w", err)
	}
	if cur, err := os.ReadFile(d.Path); err == nil {
		_ = os.WriteFile(d.Path+".bak", cur, 0o644)
	}
	tmp := d.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(raw), 0o644); err != nil {
		return fmt.Errorf("write temp preamble: %w", err)
	}
	if err := os.Rename(tmp, d.Path); err != nil {
		return fmt.Errorf("replace preamble: %w", err)
	}
	return nil
}
