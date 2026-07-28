package supervisor

import (
	"context"
	"fmt"
)

// mmdsIP is the link-local MMDS address the guest routes to over eth0.
const mmdsIP = "169.254.169.254"

// Bootstrap performs the steps that must happen before the guest knows its own network:
// mount the pseudo-filesystems, bring eth0 up, add a link-local route to MMDS, then read and
// parse the boot config. The full static network is configured afterwards from the returned
// config (PLAN §8.1).
func Bootstrap(ctx context.Context, platform Platform, runner Runner, mmdsBase string) (BootConfig, error) {
	if err := platform.MountBaseFilesystems(); err != nil {
		return BootConfig{}, fmt.Errorf("bootstrap: mount base filesystems: %w", err)
	}
	if err := runner.Run(ctx, "ip", "link", "set", "eth0", "up"); err != nil {
		return BootConfig{}, fmt.Errorf("bootstrap: eth0 up: %w", err)
	}
	if err := runner.Run(ctx, "ip", "route", "add", mmdsIP, "dev", "eth0"); err != nil {
		return BootConfig{}, fmt.Errorf("bootstrap: mmds route: %w", err)
	}
	data, err := NewMMDSClient(mmdsBase).Fetch(ctx)
	if err != nil {
		return BootConfig{}, fmt.Errorf("bootstrap: fetch mmds: %w", err)
	}
	return ParseBootConfig(data)
}
