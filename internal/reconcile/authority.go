package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/chelodo/fleet/internal/session"
)

// FileAuthority reads the expected session set from a JSON file on every pass, so an
// operator can edit it live. It is the M2 authority; M4 replaces it with one backed by
// Temporal's open workflows (PLAN §7.7).
//
// File format: {"sessions": ["s_...", "s_..."]}. A present-but-empty list means "nothing is
// expected" — every unclaimed VM is an orphan. A missing/unreadable file returns an error,
// so the reconciler fails closed (it reaps nothing rather than reaping everything).
type FileAuthority struct {
	path string
}

// NewFileAuthority returns an authority backed by the file at path.
func NewFileAuthority(path string) *FileAuthority { return &FileAuthority{path: path} }

type expectedFile struct {
	Sessions []string `json:"sessions"`
}

// ExpectedSessions reads and parses the file.
func (a *FileAuthority) ExpectedSessions(_ context.Context) (map[session.ID]struct{}, error) {
	data, err := os.ReadFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("read authority file %s: %w", a.path, err)
	}
	var ef expectedFile
	if err := json.Unmarshal(data, &ef); err != nil {
		return nil, fmt.Errorf("parse authority file %s: %w", a.path, err)
	}
	set := make(map[session.ID]struct{}, len(ef.Sessions))
	for _, s := range ef.Sessions {
		id, err := session.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("authority file %s: %w", a.path, err)
		}
		set[id] = struct{}{}
	}
	return set, nil
}
