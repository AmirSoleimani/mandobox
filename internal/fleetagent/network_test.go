package fleetagent

import (
	"testing"

	"github.com/chelodo/mandobox/internal/session"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.GuestSubnet = "172.16.0.0/12"
	cfg.HostGatewayIP = "172.31.0.1"
	return cfg
}

func TestAllocateFirstBlock(t *testing.T) {
	n := NewNetwork(testConfig(t), nil)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	g, err := n.Allocate(id, nil)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if g.HostIP != "172.16.0.1" || g.GuestIP != "172.16.0.2" {
		t.Fatalf("first block = host %s guest %s, want 172.16.0.1 / 172.16.0.2", g.HostIP, g.GuestIP)
	}
	if g.PrefixLen != 30 {
		t.Fatalf("PrefixLen = %d, want 30", g.PrefixLen)
	}
	if g.Gateway != g.HostIP {
		t.Fatalf("Gateway %s != HostIP %s", g.Gateway, g.HostIP)
	}
	if g.DNS != "172.31.0.1" {
		t.Fatalf("DNS = %s, want 172.31.0.1", g.DNS)
	}
	if g.Tap != id.TapName() {
		t.Fatalf("Tap = %s, want %s", g.Tap, id.TapName())
	}
}

func TestAllocateSkipsUsedBlocks(t *testing.T) {
	n := NewNetwork(testConfig(t), nil)
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	inUse := []VMRecord{
		{GuestIP: "172.16.0.2"}, // first /30
		{GuestIP: "172.16.0.6"}, // second /30
	}
	g, err := n.Allocate(id, inUse)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if g.GuestIP != "172.16.0.10" {
		t.Fatalf("GuestIP = %s, want 172.16.0.10 (third /30)", g.GuestIP)
	}
	if g.HostIP != "172.16.0.9" {
		t.Fatalf("HostIP = %s, want 172.16.0.9", g.HostIP)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	n := NewNetwork(testConfig(t), nil)
	// A /30 pool holds exactly one usable /30 block.
	n.cfg.GuestSubnet = "10.0.0.0/30"
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")

	inUse := []VMRecord{{GuestIP: "10.0.0.2"}}
	if _, err := n.Allocate(id, inUse); err == nil {
		t.Fatal("expected exhaustion error, got nil")
	}
}
