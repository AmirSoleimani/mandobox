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
	if got := s.harvestScreenshot(); got != nil {
		t.Fatalf("no .mando dir: want nil, got %d bytes", len(got))
	}

	if err := os.MkdirAll(mando, 0o755); err != nil {
		t.Fatal(err)
	}

	// Non-PNG files are ignored; an empty (of PNGs) dir → nil.
	write("notes.txt", []byte("not an image"), time.Now())
	if got := s.harvestScreenshot(); got != nil {
		t.Fatalf("no png present: want nil, got %d bytes", len(got))
	}

	// The most-recently-modified PNG wins (the agent's final capture).
	now := time.Now()
	write("old.png", []byte("OLD"), now.Add(-1*time.Hour))
	write("new.png", []byte("NEWEST"), now)
	if got := string(s.harvestScreenshot()); got != "NEWEST" {
		t.Fatalf("newest wins: want %q, got %q", "NEWEST", got)
	}

	// A newest-but-oversize capture is skipped (returns nil rather than shipping something huge).
	write("huge.png", make([]byte, (3<<20)+1), now.Add(time.Hour))
	if got := s.harvestScreenshot(); got != nil {
		t.Fatalf("oversize newest: want nil, got %d bytes", len(got))
	}
}
