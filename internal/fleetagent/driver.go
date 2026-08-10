package fleetagent

import (
	"context"

	"github.com/acme/mandobox/internal/session"
)

// LaunchSpec is everything a driver needs to boot one microVM. The Manager assembles it
// from the API request plus the workspace/rootfs/network it has already prepared.
type LaunchSpec struct {
	Session       session.ID
	KernelPath    string         // host path to the guest kernel (hardlinked into the chroot)
	RootfsSource  string         // decompressed golden rootfs .ext4 (reflink-copied into the chroot)
	WorkspacePath string         // persistent workspace volume (hardlinked into the chroot)
	Net           GuestNet       // allocated tap + point-to-point addressing
	VCPUs         int            // guest vCPUs
	MemMiB        int            // guest memory
	BootArgs      string         // kernel cmdline
	MMDS          map[string]any // MMDS payload injected at boot (Tier-1 tokens + net facts)
}

// LaunchResult reports the launched VM's process and chroot for state-keeping.
type LaunchResult struct {
	PID    int
	Chroot string // jail instance directory (its /root is the guest's filesystem root)
}

// VMDriver boots and tears down microVMs. FirecrackerDriver is the real implementation;
// tests use a fake so Manager lifecycle logic is exercised without KVM.
type VMDriver interface {
	Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error)
	// Destroy stops the VM process and removes its ephemeral chroot. It must NOT touch the
	// persistent workspace volume.
	Destroy(ctx context.Context, rec VMRecord) error
}
