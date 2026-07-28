package fleetagent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acme/fleet/internal/session"
)

func TestStateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	rec := VMRecord{
		Session: id, ImageSHA: "abc123", Tap: id.TapName(),
		Chroot: "/srv/jailer/x", Workspace: "/var/lib/fleet/workspaces/" + id.String() + ".ext4",
		GuestIP: "172.16.0.2", HostIP: "172.16.0.1", VCPUs: 2, MemMiB: 4096,
		PID: 4242, StartedAt: 1700000000,
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != rec {
		t.Fatalf("Get = %+v, want %+v", got, rec)
	}

	// Flat files the reaper reads (PLAN §7.6).
	for name, want := range map[string]string{
		"firecracker.pid": "4242\n",
		"started_at":      "1700000000\n",
	} {
		b, err := os.ReadFile(filepath.Join(s.Dir(id), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(b) != want {
			t.Errorf("%s = %q, want %q", name, string(b), want)
		}
	}
}

func TestStateStoreListAndDelete(t *testing.T) {
	dir := t.TempDir()
	s := NewStateStore(dir)

	ids := []session.ID{
		session.MustParse("s_00000000000000000000000000"),
		session.MustParse("s_11111111111111111111111111"),
	}
	for i, id := range ids {
		if err := s.Put(VMRecord{Session: id, PID: 100 + i, StartedAt: 1}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// A stray non-session directory must be ignored.
	if err := os.MkdirAll(filepath.Join(dir, "not-a-session"), 0o750); err != nil {
		t.Fatal(err)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d, want 2", len(list))
	}

	if err := s.Delete(ids[0]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ids[0]); err != ErrNotFound {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	list, _ = s.List()
	if len(list) != 1 {
		t.Fatalf("List after delete returned %d, want 1", len(list))
	}
}

func TestGetMissing(t *testing.T) {
	s := NewStateStore(t.TempDir())
	if _, err := s.Get(session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")); err != ErrNotFound {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}
