package fleetagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"

	"github.com/acme/mandobox/internal/session"
)

// prefixLen is the per-VM point-to-point subnet size. A /30 gives four addresses:
// network, host (.1), guest (.2), broadcast — no shared bridge.
const prefixLen = 30

// GuestNet is the network configuration mando-agent allocates for one VM. It is injected
// into MMDS so the guest supervisor can configure eth0 statically — no DHCP.
type GuestNet struct {
	Tap       string `json:"tap"`
	HostIP    string `json:"host_ip"`
	GuestIP   string `json:"guest_ip"`
	PrefixLen int    `json:"prefix_len"`
	Gateway   string `json:"gateway"` // == HostIP; guest's default route
	DNS       string `json:"dns"`     // host resolver anchor
}

// Network creates and tears down per-VM tap devices and allocates their addresses.
type Network struct {
	cfg    Config
	runner Runner
}

// NewNetwork returns a Network using the given runner for ip(8) operations.
func NewNetwork(cfg Config, runner Runner) *Network { return &Network{cfg: cfg, runner: runner} }

// Allocate picks the first free /30 in GuestSubnet not used by an existing VM and returns
// the network config for id. It is pure (no host mutation) so allocation is unit-testable.
func (n *Network) Allocate(id session.ID, inUse []VMRecord) (GuestNet, error) {
	pool, err := netip.ParsePrefix(n.cfg.GuestSubnet)
	if err != nil {
		return GuestNet{}, fmt.Errorf("allocate: bad GuestSubnet %q: %w", n.cfg.GuestSubnet, err)
	}
	pool = pool.Masked()
	if !pool.Addr().Is4() {
		return GuestNet{}, fmt.Errorf("allocate: GuestSubnet must be IPv4, got %s", pool)
	}

	used := make(map[netip.Addr]bool, len(inUse))
	for _, r := range inUse {
		if a, err := netip.ParseAddr(r.GuestIP); err == nil {
			used[a] = true
		}
	}

	base := pool.Addr()
	for {
		host := addrAdd(base, 1)
		guest := addrAdd(base, 2)
		bcast := addrAdd(base, 3)
		if !pool.Contains(base) || !pool.Contains(bcast) {
			return GuestNet{}, fmt.Errorf("allocate: no free /%d in %s (pool exhausted)", prefixLen, pool)
		}
		if !used[guest] {
			return GuestNet{
				Tap:       id.TapName(),
				HostIP:    host.String(),
				GuestIP:   guest.String(),
				PrefixLen: prefixLen,
				Gateway:   host.String(),
				DNS:       n.cfg.HostGatewayIP,
			}, nil
		}
		base = addrAdd(base, 4) // next /30 block
	}
}

// CreateTap creates the tap device, assigns the host side of the point-to-point link, and
// brings it up. Idempotent-ish: an existing tap of the same name is removed first.
func (n *Network) CreateTap(ctx context.Context, g GuestNet) error {
	_ = n.DeleteTap(ctx, g.Tap) // best effort: clear any stale tap of this name
	if err := n.runner.Run(ctx, "ip", "tuntap", "add", "dev", g.Tap, "mode", "tap"); err != nil {
		return fmt.Errorf("create tap %s: %w", g.Tap, err)
	}
	cidr := fmt.Sprintf("%s/%d", g.HostIP, g.PrefixLen)
	if err := n.runner.Run(ctx, "ip", "addr", "add", cidr, "dev", g.Tap); err != nil {
		return fmt.Errorf("addr add %s: %w", cidr, err)
	}
	if err := n.runner.Run(ctx, "ip", "link", "set", "dev", g.Tap, "up"); err != nil {
		return fmt.Errorf("link up %s: %w", g.Tap, err)
	}
	return nil
}

// DeleteTap removes a tap device. Missing devices are not an error.
func (n *Network) DeleteTap(ctx context.Context, tap string) error {
	if tap == "" {
		return nil
	}
	// `ip link del` fails if absent; callers treat teardown as best-effort.
	return n.runner.Run(ctx, "ip", "link", "del", "dev", tap)
}

// addrAdd returns a + n for an IPv4 address.
func addrAdd(a netip.Addr, n uint32) netip.Addr {
	b := a.As4()
	v := binary.BigEndian.Uint32(b[:]) + n
	binary.BigEndian.PutUint32(b[:], v)
	return netip.AddrFrom4(b)
}
