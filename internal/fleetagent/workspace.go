package fleetagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/chelodo/fleet/internal/session"
)

// imageSHAPattern constrains image identifiers to a hex digest, so they can never be used
// for path traversal when composing the golden-image filename.
var imageSHAPattern = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// Workspace manages the persistent per-session volume and the golden rootfs it boots from.
// The workspace is the only mutable state that survives a VM (PLAN §7.2, I7).
type Workspace struct {
	cfg    Config
	runner Runner
}

// NewWorkspace returns a Workspace using runner for fallocate/mkfs/cp/zstd.
func NewWorkspace(cfg Config, runner Runner) *Workspace {
	return &Workspace{cfg: cfg, runner: runner}
}

// Path returns the persistent workspace image path for id.
func (w *Workspace) Path(id session.ID) string {
	return filepath.Join(w.cfg.WorkspacesDir, id.String()+".ext4")
}

// Ensure returns the workspace path, creating and formatting an ext4 volume on first use
// and reusing it thereafter (this is what makes a fresh VM resume an existing session —
// §8.1). created reports whether it was freshly made.
func (w *Workspace) Ensure(ctx context.Context, id session.ID, sizeMiB int) (path string, created bool, err error) {
	if !id.Valid() {
		return "", false, fmt.Errorf("ensure workspace: invalid session_id %q", id)
	}
	path = w.Path(id)
	if _, err := os.Stat(path); err == nil {
		return path, false, nil // reuse
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("ensure workspace: stat: %w", err)
	}
	if sizeMiB <= 0 {
		sizeMiB = w.cfg.WorkspaceSizeMiB
	}
	if err := w.runner.Run(ctx, "fallocate", "-l", strconv.Itoa(sizeMiB)+"MiB", path); err != nil {
		return "", false, fmt.Errorf("ensure workspace: fallocate: %w", err)
	}
	if err := w.runner.Run(ctx, "mkfs.ext4", "-F", "-q", path); err != nil {
		_ = os.Remove(path)
		return "", false, fmt.Errorf("ensure workspace: mkfs: %w", err)
	}
	// The jailed Firecracker (fleet uid) must be able to open the volume.
	if err := w.runner.Run(ctx, "chown", fmt.Sprintf("%d:%d", w.cfg.JailUID, w.cfg.JailGID), path); err != nil {
		return "", false, fmt.Errorf("ensure workspace: chown: %w", err)
	}
	return path, true, nil
}

// Destroy discards the workspace's blocks and unlinks it. Discarding (punch-hole) before
// unlink limits how long a secret that appeared once in the session transcript stays
// recoverable on disk (PLAN §6.1 DestroyWorkspace, §9 transcript leakage).
func (w *Workspace) Destroy(ctx context.Context, id session.ID) error {
	if !id.Valid() {
		return fmt.Errorf("destroy workspace: invalid session_id %q", id)
	}
	path := w.Path(id)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("destroy workspace: stat: %w", err)
	}
	if size := info.Size(); size > 0 {
		if err := w.runner.Run(ctx, "fallocate", "--punch-hole", "--offset", "0",
			"--length", strconv.FormatInt(size, 10), path); err != nil {
			return fmt.Errorf("destroy workspace: discard blocks: %w", err)
		}
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("destroy workspace: unlink: %w", err)
	}
	return nil
}

// RootfsExt4Path returns the decompressed golden rootfs path for an image sha.
func (w *Workspace) RootfsExt4Path(imageSHA string) string {
	return filepath.Join(w.cfg.ImagesDir, "rootfs-"+imageSHA+".ext4")
}

// EnsureRootfs makes sure the decompressed golden rootfs for imageSHA exists, decompressing
// rootfs-<sha>.ext4.zst on first use (the cache ships compressed, §7.2). Returns the .ext4
// path to reflink-copy per launch.
func (w *Workspace) EnsureRootfs(ctx context.Context, imageSHA string) (string, error) {
	if !imageSHAPattern.MatchString(imageSHA) {
		return "", fmt.Errorf("ensure rootfs: invalid image sha %q", imageSHA)
	}
	ext4 := w.RootfsExt4Path(imageSHA)
	if _, err := os.Stat(ext4); err == nil {
		return ext4, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("ensure rootfs: stat: %w", err)
	}
	zst := ext4 + ".zst"
	if _, err := os.Stat(zst); err != nil {
		return "", fmt.Errorf("ensure rootfs: golden image %s not present: %w", filepath.Base(zst), err)
	}
	if err := w.runner.Run(ctx, "zstd", "-d", "--keep", "-o", ext4, zst); err != nil {
		return "", fmt.Errorf("ensure rootfs: decompress: %w", err)
	}
	return ext4, nil
}

// ReflinkCopy copy-on-write clones src to dst. Requires src and dst on the same
// reflink-capable filesystem (XFS/Btrfs); on ext4 or across filesystems cp fails and the
// error surfaces to the operator (see docs/hetzner-setup.md §3).
func (w *Workspace) ReflinkCopy(ctx context.Context, src, dst string) error {
	if err := w.runner.Run(ctx, "cp", "--reflink=always", src, dst); err != nil {
		return fmt.Errorf("reflink copy %s -> %s: %w", src, dst, err)
	}
	return nil
}
