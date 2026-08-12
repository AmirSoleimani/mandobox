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
	turnStart := time.Now()

	write := func(name string, content []byte, mod time.Time) {
		t.Helper()
		p := filepath.Join(mando, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
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

	// Non-PNG files are ignored; no PNG → nil.
	write("notes.txt", []byte("not an image"), turnStart.Add(time.Second))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("no png present: want nil, got %d bytes", len(got))
	}

	// A PNG from BEFORE this turn is stale and must not be harvested (would re-post an earlier turn's).
	write("stale.png", []byte("STALE"), turnStart.Add(-time.Hour))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("pre-turn png: want nil, got %q", string(got))
	}

	// Among this-turn PNGs, the most-recent (the agent's final capture) wins.
	write("first.png", []byte("FIRST"), turnStart.Add(1*time.Second))
	write("final.png", []byte("FINAL"), turnStart.Add(2*time.Second))
	if got := string(s.harvestScreenshot(turnStart)); got != "FINAL" {
		t.Fatalf("newest wins: want %q, got %q", "FINAL", got)
	}

	// A symlink is never followed, even if it is the newest ".png" entry — no exfiltration via .mando.
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(mando, "zzz-newest.png")); err != nil {
		t.Fatal(err)
	}
	if got := string(s.harvestScreenshot(turnStart)); got != "FINAL" {
		t.Fatalf("symlink must be skipped: want %q, got %q", "FINAL", got)
	}

	// A newest-but-oversize capture is skipped (returns nil rather than shipping something over the cap).
	write("huge.png", make([]byte, (1<<20)+1), turnStart.Add(3*time.Second))
	if got := s.harvestScreenshot(turnStart); got != nil {
		t.Fatalf("oversize newest: want nil, got %d bytes", len(got))
	}
}
