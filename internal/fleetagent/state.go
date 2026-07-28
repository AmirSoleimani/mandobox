package fleetagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/acme/fleet/internal/session"
)

// ErrNotFound is returned when no state exists for a session.
var ErrNotFound = errors.New("vm state not found")

// VMRecord is the authoritative per-VM state fleet-agent maintains under
// RunStateDir/<session_id>/. It is the reaper's and reconciler's view of a live VM
// (PLAN §7.6, §7.7). The directory lives on tmpfs, so a host reboot clears it while the
// workspace volume (real state, I7) survives elsewhere.
type VMRecord struct {
	Session   session.ID `json:"session_id"`
	ImageSHA  string     `json:"image_sha"`
	Tap       string     `json:"tap"`
	Chroot    string     `json:"chroot"`
	Workspace string     `json:"workspace"`
	GuestIP   string     `json:"guest_ip"`
	HostIP    string     `json:"host_ip"`
	VCPUs     int        `json:"vcpus"`
	MemMiB    int        `json:"mem_mib"`
	PID       int        `json:"pid"`
	StartedAt int64      `json:"started_at"` // epoch seconds
}

// StateStore persists VMRecords, one directory per session. It writes the two flat files
// the host reaper reads (firecracker.pid, started_at) alongside the rich vm.json, so the
// reaper never has to parse JSON.
type StateStore struct {
	dir string
}

// NewStateStore returns a store rooted at dir (typically Config.RunStateDir).
func NewStateStore(dir string) *StateStore { return &StateStore{dir: dir} }

// Dir returns the per-session state directory path.
func (s *StateStore) Dir(id session.ID) string { return filepath.Join(s.dir, id.String()) }

// Put writes rec atomically. The vm.json is written to a temp file and renamed so a
// concurrent reader never sees a partial record.
func (s *StateStore) Put(rec VMRecord) error {
	if !rec.Session.Valid() {
		return fmt.Errorf("put: invalid session_id %q", rec.Session)
	}
	dir := s.Dir(rec.Session)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("put: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("put: marshal: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "vm.json"), append(data, '\n'), 0o640); err != nil {
		return err
	}
	// Flat files for the reaper (PLAN §7.6).
	if err := writeFileAtomic(filepath.Join(dir, "firecracker.pid"), []byte(strconv.Itoa(rec.PID)+"\n"), 0o640); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "started_at"), []byte(strconv.FormatInt(rec.StartedAt, 10)+"\n"), 0o640); err != nil {
		return err
	}
	return nil
}

// Get returns the record for id, or ErrNotFound.
func (s *StateStore) Get(id session.ID) (VMRecord, error) {
	data, err := os.ReadFile(filepath.Join(s.Dir(id), "vm.json"))
	if errors.Is(err, os.ErrNotExist) {
		return VMRecord{}, ErrNotFound
	}
	if err != nil {
		return VMRecord{}, fmt.Errorf("get: read: %w", err)
	}
	var rec VMRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return VMRecord{}, fmt.Errorf("get: unmarshal %s: %w", id, err)
	}
	return rec, nil
}

// List returns every recorded VM, sorted by session_id. Records with an unreadable or
// malformed vm.json are skipped (they are transient scratch, not authoritative).
func (s *StateStore) List() ([]VMRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list: readdir: %w", err)
	}
	var out []VMRecord
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := session.Parse(e.Name())
		if err != nil {
			continue // not a session dir
		}
		rec, err := s.Get(id)
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}

// Delete removes the runtime state directory for id. It never touches the workspace
// volume — that lives under WorkspacesDir and may back an open PR (PLAN §7.6, I7).
func (s *StateStore) Delete(id session.ID) error {
	if err := os.RemoveAll(s.Dir(id)); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}

// writeFileAtomic writes data to a temp file in the same directory then renames it over
// path, so readers never observe a partial write.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("atomic write: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: write: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("atomic write: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomic write: rename: %w", err)
	}
	tmpName = "" // renamed; don't remove
	return nil
}
