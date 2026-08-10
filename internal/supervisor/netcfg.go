package supervisor

import (
	"context"
	"fmt"
	"os"
)

// ConfigureNetwork brings up lo and eth0 with the static point-to-point address mando-agent
// allocated, sets the default route, and writes resolv.conf — no DHCP. resolv is
// the path to write (real /etc/resolv.conf in the guest; a temp file in tests).
func ConfigureNetwork(ctx context.Context, runner Runner, net NetworkConfig, resolv string) error {
	cidr := fmt.Sprintf("%s/%d", net.GuestIP, net.PrefixLen)
	steps := [][]string{
		{"link", "set", "lo", "up"},
		{"addr", "add", cidr, "dev", "eth0"},
		{"link", "set", "eth0", "up"},
		{"route", "add", "default", "via", net.Gateway},
	}
	for _, s := range steps {
		if err := runner.Run(ctx, "ip", s...); err != nil {
			return fmt.Errorf("configure network (ip %v): %w", s, err)
		}
	}
	if net.DNS != "" {
		if err := os.WriteFile(resolv, []byte("nameserver "+net.DNS+"\n"), 0o644); err != nil {
			return fmt.Errorf("configure network: write resolv.conf: %w", err)
		}
	}
	return nil
}
