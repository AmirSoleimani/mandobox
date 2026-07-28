package fleetagent

// Config is fleet-agent's host configuration. Defaults mirror ansible/group_vars/fleet.yml
// so a hand-run agent and an Ansible-deployed one agree. fleet-agent is thin by design
// (PLAN §7.1): it holds no policy, only the paths and limits it needs to place a microVM.
type Config struct {
	// Paths (PLAN §7, I7).
	ImagesDir     string // golden rootfs cache (content-addressed, read-only)
	WorkspacesDir string // persistent per-session volumes — the only state that survives a VM
	KernelsDir    string // guest kernels
	KernelPath    string // default guest kernel (vmlinux)
	JailDir       string // jailer chroot base
	RunStateDir   string // per-VM runtime state, tmpfs (/run/fleet/vms)

	// Binaries.
	FirecrackerBin string
	JailerBin      string

	// Jailed identity: the uid/gid the jailer drops Firecracker to.
	JailUID int
	JailGID int

	// Network (PLAN §7.4). Point-to-point /30s are carved from GuestSubnet; HostGatewayIP
	// is the stable anchor guests reach for DNS and the egress proxy.
	GuestSubnet   string
	HostGatewayIP string

	// Capacity: refuse to launch beyond this many concurrent VMs (§7.1 EX_TEMPFAIL).
	MaxVMs int

	// Default workspace volume size when a session's volume is first created.
	WorkspaceSizeMiB int
}

// DefaultConfig returns the built-in defaults matching group_vars/fleet.yml.
func DefaultConfig() Config {
	return Config{
		ImagesDir:        "/var/lib/fleet/images",
		WorkspacesDir:    "/var/lib/fleet/workspaces",
		KernelsDir:       "/var/lib/fleet/kernels",
		KernelPath:       "/var/lib/fleet/kernels/vmlinux",
		JailDir:          "/srv/jailer",
		RunStateDir:      "/run/fleet/vms",
		FirecrackerBin:   "/usr/local/bin/firecracker",
		JailerBin:        "/usr/local/bin/jailer",
		JailUID:          2000,
		JailGID:          2000,
		GuestSubnet:      "172.16.0.0/12",
		HostGatewayIP:    "172.31.0.1",
		MaxVMs:           8,
		WorkspaceSizeMiB: 8192,
	}
}
