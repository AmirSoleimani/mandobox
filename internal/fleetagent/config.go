package fleetagent

// Config is mando-agent's host configuration. Defaults mirror ansible/group_vars/fleet.yml
// so a hand-run agent and an Ansible-deployed one agree. mando-agent is thin by design:
// it holds no policy, only the paths and limits it needs to place a microVM.
type Config struct {
	// Paths.
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

	// Network. Point-to-point /30s are carved from GuestSubnet; HostGatewayIP
	// is the stable anchor guests reach for DNS and the egress proxy.
	GuestSubnet   string
	HostGatewayIP string

	// Capacity: refuse to launch beyond this many concurrent VMs (EX_TEMPFAIL).
	MaxVMs int

	// Default workspace volume size when a session's volume is first created.
	WorkspaceSizeMiB int

	// DriveBandwidthMBps caps each VM's per-drive block I/O (a Firecracker rate_limiter), so one
	// guest can't saturate host disk and starve co-resident sessions (noisy-neighbor). 0 = unlimited.
	DriveBandwidthMBps int
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
		// Generous enough not to slow real builds (npm/cargo/apt), tight enough to bound a runaway
		// guest's host disk impact. Tune down for denser packing, or 0 to disable.
		DriveBandwidthMBps: 250,
	}
}
