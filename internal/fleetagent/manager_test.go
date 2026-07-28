package fleetagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/acme/fleet/internal/session"
)

type fakeDriver struct {
	pid        int
	failLaunch bool
	launched   []session.ID
	destroyed  []session.ID
	lastSpec   LaunchSpec
}

func (d *fakeDriver) Launch(_ context.Context, spec LaunchSpec) (LaunchResult, error) {
	d.lastSpec = spec
	if d.failLaunch {
		return LaunchResult{}, errors.New("boom")
	}
	d.pid++
	d.launched = append(d.launched, spec.Session)
	return LaunchResult{PID: 1000 + d.pid, Chroot: "/srv/jailer/firecracker/" + spec.Session.String()}, nil
}

func (d *fakeDriver) Destroy(_ context.Context, rec VMRecord) error {
	d.destroyed = append(d.destroyed, rec.Session)
	return nil
}

func newTestManager(t *testing.T, maxVMs int) (*Manager, *fakeDriver, *fakeRunner) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.RunStateDir = t.TempDir()
	cfg.WorkspacesDir = t.TempDir()
	cfg.ImagesDir = t.TempDir()
	cfg.MaxVMs = maxVMs
	// A golden rootfs must be present for EnsureRootfs.
	if err := os.WriteFile(filepath.Join(cfg.ImagesDir, "rootfs-deadbeefcafebabe.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fr := newFakeRunner()
	fd := &fakeDriver{}
	return NewManager(cfg, fr, fd, nil), fd, fr
}

func mustLaunch(t *testing.T, m *Manager, id session.ID) VMRecord {
	t.Helper()
	rec, err := m.Launch(context.Background(), LaunchRequest{Session: id, ImageSHA: "deadbeefcafebabe"})
	if err != nil {
		t.Fatalf("Launch(%s): %v", id, err)
	}
	return rec
}

func TestLaunchSuccess(t *testing.T) {
	m, fd, fr := newTestManager(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	rec := mustLaunch(t, m, id)
	if rec.PID == 0 || rec.Tap != id.TapName() || rec.GuestIP != "172.16.0.2" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if got, err := m.store.Get(id); err != nil || got.PID != rec.PID {
		t.Fatalf("state not persisted: %+v err=%v", got, err)
	}
	if !fr.ran("ip tuntap add dev " + id.TapName()) {
		t.Error("tap was not created")
	}
	// MMDS must carry the allocated network and the session_id (§8.1).
	if fd.lastSpec.MMDS["session_id"] != id.String() {
		t.Error("MMDS missing session_id")
	}
	if _, ok := fd.lastSpec.MMDS["network"]; !ok {
		t.Error("MMDS missing network facts")
	}
}

func TestLaunchIdempotent(t *testing.T) {
	m, fd, _ := newTestManager(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	orig := procAlive
	procAlive = func(int) bool { return true }
	t.Cleanup(func() { procAlive = orig })

	first := mustLaunch(t, m, id)
	second := mustLaunch(t, m, id)
	if first.PID != second.PID {
		t.Fatalf("relaunch changed PID: %d -> %d", first.PID, second.PID)
	}
	if len(fd.launched) != 1 {
		t.Fatalf("driver.Launch called %d times, want 1", len(fd.launched))
	}
}

func TestLaunchAtCapacity(t *testing.T) {
	m, _, _ := newTestManager(t, 1)
	mustLaunch(t, m, session.MustParse("s_00000000000000000000000000"))
	_, err := m.Launch(context.Background(), LaunchRequest{
		Session: session.MustParse("s_11111111111111111111111111"), ImageSHA: "deadbeefcafebabe",
	})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("second launch err = %v, want ErrAtCapacity", err)
	}
}

func TestLaunchForbiddenMMDS(t *testing.T) {
	m, fd, _ := newTestManager(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	_, err := m.Launch(context.Background(), LaunchRequest{
		Session: id, ImageSHA: "deadbeefcafebabe",
		MMDS: map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": "sk-secret"}},
	})
	if !errors.Is(err, ErrForbiddenMMDS) {
		t.Fatalf("err = %v, want ErrForbiddenMMDS", err)
	}
	if len(fd.launched) != 0 {
		t.Fatal("driver launched despite forbidden MMDS")
	}
}

func TestDestroyKeepsWorkspace(t *testing.T) {
	m, fd, _ := newTestManager(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	mustLaunch(t, m, id)

	// Simulate the persistent volume on disk (the fake runner doesn't really fallocate).
	wsPath := m.ws.Path(id)
	if err := os.WriteFile(wsPath, []byte("workspace"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := m.Destroy(context.Background(), id, false); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(wsPath); err != nil {
		t.Fatalf("workspace was removed on non-purge destroy: %v", err)
	}
	if _, err := m.store.Get(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state survived destroy: %v", err)
	}
	if len(fd.destroyed) != 1 {
		t.Fatalf("driver.Destroy called %d times, want 1", len(fd.destroyed))
	}
}

func TestDestroyPurgesWorkspace(t *testing.T) {
	m, _, _ := newTestManager(t, 8)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	mustLaunch(t, m, id)
	wsPath := m.ws.Path(id)
	if err := os.WriteFile(wsPath, []byte("workspace"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := m.Destroy(context.Background(), id, true); err != nil {
		t.Fatalf("Destroy purge: %v", err)
	}
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Fatalf("workspace survived purge destroy: %v", err)
	}
}

func TestDestroyUnknownSessionIsNoop(t *testing.T) {
	m, _, _ := newTestManager(t, 8)
	if err := m.Destroy(context.Background(), session.MustParse("s_0123456789ABCDEFGHJKMNPQRS"), false); err != nil {
		t.Fatalf("destroy unknown = %v, want nil", err)
	}
}
