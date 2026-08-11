package fleetagent

import (
	"context"
	"os"
	"testing"

	"github.com/AmirSoleimani/mandobox/internal/session"
)

func TestWorkspaceEnsureReuse(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkspacesDir = t.TempDir()
	fr := newFakeRunner()
	w := NewWorkspace(cfg, fr)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	// Pre-create the volume: Ensure must reuse it and touch no external commands.
	if err := os.WriteFile(w.Path(id), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	path, created, err := w.Ensure(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if created {
		t.Fatal("Ensure reported created=true for an existing volume")
	}
	if path != w.Path(id) {
		t.Fatalf("Ensure path = %s, want %s", path, w.Path(id))
	}
	if fr.ran("mkfs.ext4") {
		t.Fatal("Ensure formatted an existing volume")
	}
}

func TestWorkspaceEnsureCreate(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkspacesDir = t.TempDir()
	fr := newFakeRunner()
	w := NewWorkspace(cfg, fr)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	if _, created, err := w.Ensure(context.Background(), id, 1024); err != nil || !created {
		t.Fatalf("Ensure: created=%v err=%v, want created=true nil", created, err)
	}
	if !fr.ran("fallocate -l 1024MiB") {
		t.Error("Ensure did not fallocate the requested size")
	}
	if !fr.ran("mkfs.ext4 -F -q") {
		t.Error("Ensure did not format the volume")
	}
	if !fr.ran("chown 2000:2000") {
		t.Error("Ensure did not chown to the jailed uid/gid")
	}
}

func TestEnsureRootfsValidatesSHA(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ImagesDir = t.TempDir()
	w := NewWorkspace(cfg, newFakeRunner())

	if _, err := w.EnsureRootfs(context.Background(), "../etc/passwd"); err == nil {
		t.Fatal("EnsureRootfs accepted a path-traversal sha")
	}
	if _, err := w.EnsureRootfs(context.Background(), "deadbeefdeadbeef"); err == nil {
		t.Fatal("EnsureRootfs accepted a sha with no image present")
	}
}
