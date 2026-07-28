package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acme/fleet/internal/session"
)

type destroyCall struct {
	id    session.ID
	purge bool
}

type fakeClient struct {
	vms       []VM
	destroyed []destroyCall
}

func (c *fakeClient) List(context.Context) ([]VM, error) { return c.vms, nil }
func (c *fakeClient) Destroy(_ context.Context, id session.ID, purge bool) error {
	c.destroyed = append(c.destroyed, destroyCall{id, purge})
	return nil
}

type fakeAuthority map[session.ID]struct{}

func (a fakeAuthority) ExpectedSessions(context.Context) (map[session.ID]struct{}, error) {
	return a, nil
}

var fixedNow = func() time.Time { return time.Unix(1_000_000, 0) }

func TestReconcileReapsUnclaimedOrphan(t *testing.T) {
	orphan := session.MustParse("s_00000000000000000000000000")
	claimed := session.MustParse("s_11111111111111111111111111")

	client := &fakeClient{vms: []VM{
		{Session: orphan, StartedAt: fixedNow().Unix() - 3600},  // old, unclaimed → reap
		{Session: claimed, StartedAt: fixedNow().Unix() - 3600}, // old, claimed → keep
	}}
	auth := fakeAuthority{claimed: {}}
	r := New(client, auth, Options{Grace: time.Minute, Now: fixedNow})

	killed, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(killed) != 1 || killed[0] != orphan {
		t.Fatalf("killed = %v, want [%s]", killed, orphan)
	}
	if len(client.destroyed) != 1 || client.destroyed[0].id != orphan {
		t.Fatalf("destroyed = %+v", client.destroyed)
	}
	// Orphans keep their workspace (I7).
	if client.destroyed[0].purge {
		t.Error("orphan was destroyed with purge_workspace=true; must keep the volume")
	}
}

func TestReconcileSkipsYoungOrphan(t *testing.T) {
	young := session.MustParse("s_00000000000000000000000000")
	client := &fakeClient{vms: []VM{
		{Session: young, StartedAt: fixedNow().Unix() - 10}, // younger than grace
	}}
	r := New(client, fakeAuthority{}, Options{Grace: time.Minute, Now: fixedNow})

	killed, err := r.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(killed) != 0 {
		t.Fatalf("killed %v, want none (within grace)", killed)
	}
}

func TestFileAuthority(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "expected.json")

	// Missing file → error (fail closed).
	if _, err := NewFileAuthority(path).ExpectedSessions(context.Background()); err == nil {
		t.Fatal("missing authority file should error")
	}

	if err := os.WriteFile(path, []byte(`{"sessions":["s_00000000000000000000000000"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	set, err := NewFileAuthority(path).ExpectedSessions(context.Background())
	if err != nil {
		t.Fatalf("ExpectedSessions: %v", err)
	}
	if _, ok := set[session.MustParse("s_00000000000000000000000000")]; !ok || len(set) != 1 {
		t.Fatalf("set = %v", set)
	}
}
