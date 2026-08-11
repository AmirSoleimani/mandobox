package fleetagent

import (
	"testing"

	"github.com/AmirSoleimani/mandobox/internal/session"
)

func TestDeriveMAC(t *testing.T) {
	cases := map[string]string{
		"172.16.0.2": "06:00:ac:10:00:02",
		"10.0.0.6":   "06:00:0a:00:00:06",
		"bogus":      "06:00:00:00:00:00",
	}
	for ip, want := range cases {
		if got := deriveMAC(ip); got != want {
			t.Errorf("deriveMAC(%q) = %q, want %q", ip, got, want)
		}
	}
}

func TestInstanceDir(t *testing.T) {
	cfg := DefaultConfig()
	cfg.JailDir = "/srv/jailer"
	d := NewFirecrackerDriver(cfg, newFakeRunner())
	id := session.MustParse("s_0123456789ABCDEFGHJKMNPQRS")
	// jailer rejects underscores, so the chroot dir uses the hyphenated id.
	want := "/srv/jailer/firecracker/s-0123456789ABCDEFGHJKMNPQRS"
	if got := d.instanceDir(id); got != want {
		t.Errorf("instanceDir = %q, want %q", got, want)
	}
}
