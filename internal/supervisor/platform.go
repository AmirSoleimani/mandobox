package supervisor

// Platform abstracts the OS-privileged operations PID 1 performs — mounting pseudo and
// workspace filesystems, syncing, powering off — so orchestration is unit-testable off
// Linux with a fake. The real implementation is Linux-only (platform_linux.go).
type Platform interface {
	MountBaseFilesystems() error
	MountWorkspace(dev, target string) error
	UnmountWorkspace(target string) error
	Sync()
	PowerOff() error
}

// NewPlatform returns the platform implementation for the current OS.
func NewPlatform() Platform { return newPlatform() }
