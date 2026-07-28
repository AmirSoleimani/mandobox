package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigureNetwork(t *testing.T) {
	fr := newFakeRunner()
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	net := NetworkConfig{GuestIP: "172.16.0.2", PrefixLen: 30, Gateway: "172.16.0.1", DNS: "172.31.0.1"}

	if err := ConfigureNetwork(context.Background(), fr, net, resolv); err != nil {
		t.Fatalf("ConfigureNetwork: %v", err)
	}
	for _, want := range []string{
		"ip addr add 172.16.0.2/30 dev eth0",
		"ip link set eth0 up",
		"ip route add default via 172.16.0.1",
	} {
		if !fr.ran(want) {
			t.Errorf("missing command: %q", want)
		}
	}
	b, err := os.ReadFile(resolv)
	if err != nil {
		t.Fatalf("read resolv: %v", err)
	}
	if string(b) != "nameserver 172.31.0.1\n" {
		t.Errorf("resolv.conf = %q", string(b))
	}
}
