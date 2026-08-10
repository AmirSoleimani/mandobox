//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type linuxPlatform struct{}

func newPlatform() Platform { return linuxPlatform{} }

// MountBaseFilesystems mounts the pseudo-filesystems PID 1 needs. /dev may already be
// a devtmpfs the kernel mounted; EBUSY there is not an error.
func (linuxPlatform) MountBaseFilesystems() error {
	mounts := []struct {
		source, target, fstype string
	}{
		{"proc", "/proc", "proc"},
		{"sysfs", "/sys", "sysfs"},
		{"devtmpfs", "/dev", "devtmpfs"},
		{"tmpfs", "/run", "tmpfs"},
		{"tmpfs", "/tmp", "tmpfs"},
	}
	for _, m := range mounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", m.target, err)
		}
		if err := unix.Mount(m.source, m.target, m.fstype, 0, ""); err != nil {
			if errors.Is(err, unix.EBUSY) {
				continue
			}
			return fmt.Errorf("mount %s: %w", m.target, err)
		}
	}
	return nil
}

// MountWorkspace mounts the persistent workspace volume.
func (linuxPlatform) MountWorkspace(dev, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", target, err)
	}
	if err := unix.Mount(dev, target, "ext4", 0, ""); err != nil {
		return fmt.Errorf("mount workspace %s -> %s: %w", dev, target, err)
	}
	return nil
}

func (linuxPlatform) UnmountWorkspace(target string) error {
	return unix.Unmount(target, 0)
}

func (linuxPlatform) Sync() { unix.Sync() }

// PowerOff syncs and stops the microVM. It issues a RESTART, not POWER_OFF: Firecracker
// exits when the guest resets (with reboot=k on the cmdline, a keyboard reset) but merely
// halts — leaving the VMM process alive — on power-off. Exiting lets the reaper clean up the
// process, tap, and state.
func (linuxPlatform) PowerOff() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}
