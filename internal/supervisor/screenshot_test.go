package supervisor

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHarvestScreenshot(t *testing.T) {
	dir := t.TempDir()
	s := &Supervisor{repoDir: dir, deps: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	mando := filepath.Join(dir, ".mando")
	share := filepath.Join(mando, "share.png")
	turnStart := time.Now()

	write := func(path string, content []byte, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	// No .mando directory yet → nil (best-effort, never errors).
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("no .mando dir: want nil, got %d bytes", len(got))
	}
	if err := os.MkdirAll(mando, 0o755); err != nil {
		t.Fatal(err)
	}

	// Self-verification captures under other names are NOT shared — only .mando/share.png is.
	write(filepath.Join(mando, "check.png"), []byte("VERIFY-ONLY"), turnStart.Add(time.Second))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("non-share capture: want nil, got %q", string(got))
	}

	// A share.png left over from BEFORE this turn is stale and must not be re-posted.
	write(share, []byte("STALE-SHARE"), turnStart.Add(-time.Hour))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("pre-turn share.png: want nil, got %q", string(got))
	}

	// share.png (re)written this turn is harvested — the agent opted in and it reflects the current state.
	write(share, []byte("SHARE-ME"), turnStart.Add(2*time.Second))
	if got := string(s.harvestScreenshot(turnStart)); got != "SHARE-ME" {
		t.Fatalf("this-turn share.png: want %q, got %q", "SHARE-ME", got)
	}

	// A symlink at share.png is never followed (no exfiltration of a token file).
	os.Remove(share)
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, share); err != nil {
		t.Fatal(err)
	}
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("symlink share.png: want nil, got %q", string(got))
	}

	// An oversize share.png is dropped (never ship something over the cap).
	os.Remove(share)
	write(share, make([]byte, (1<<20)+1), turnStart.Add(3*time.Second))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("oversize share.png: want nil, got %d bytes", len(got))
	}
}
