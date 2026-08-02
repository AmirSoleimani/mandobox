package fleetagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/chelodo/fleet/internal/session"
)

// socketWait is how long to wait for Firecracker's API socket to appear after jailer start.
const socketWait = 10 * time.Second

// defaultBootArgs boots to the rootfs's own init. The golden image (M3) makes
// fc-supervisor PID 1; a test rootfs used to validate M2 supplies its own init.
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 pci=off"

// FirecrackerDriver launches microVMs under jailer and configures them over the
// Firecracker API socket. This is the KVM-dependent path (PLAN §7.1); its lifecycle is
// exercised in tests through the VMDriver interface with a fake.
type FirecrackerDriver struct {
	cfg    Config
	runner Runner
}

// NewFirecrackerDriver returns a driver using runner for reflink copies.
func NewFirecrackerDriver(cfg Config, runner Runner) *FirecrackerDriver {
	return &FirecrackerDriver{cfg: cfg, runner: runner}
}

func (d *FirecrackerDriver) instanceDir(id session.ID) string {
	return filepath.Join(d.cfg.JailDir, "firecracker", id.String())
}

// Launch runs the PLAN §7.1 sequence: jailer → place resources in the chroot → configure
// boot source, drives, network, machine, MMDS → InstantStart.
func (d *FirecrackerDriver) Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error) {
	if !spec.Session.Valid() {
		return LaunchResult{}, fmt.Errorf("launch: invalid session_id %q", spec.Session)
	}
	instance := d.instanceDir(spec.Session)
	root := filepath.Join(instance, "root")
	sock := filepath.Join(root, "firecracker.socket")

	// Fresh chroot each boot — no snapshots, no reused state (PLAN §4.2).
	if err := os.RemoveAll(instance); err != nil {
		return LaunchResult{}, fmt.Errorf("launch: clear stale chroot: %w", err)
	}

	pid, err := d.startJailer(spec.Session)
	if err != nil {
		return LaunchResult{}, err
	}
	// From here on, tear down the process if we fail to finish configuring.
	ok := false
	defer func() {
		if !ok {
			killProcess(pid, 3*time.Second)
			_ = os.RemoveAll(instance)
		}
	}()

	if err := waitForSocket(sock, socketWait); err != nil {
		return LaunchResult{}, fmt.Errorf("launch: %w", err)
	}
	if err := d.placeResources(ctx, spec, root); err != nil {
		return LaunchResult{}, err
	}
	if err := d.configure(ctx, spec, sock); err != nil {
		return LaunchResult{}, err
	}

	ok = true
	return LaunchResult{PID: pid, Chroot: instance}, nil
}

// startJailer starts jailer, which execs into Firecracker in place (so the recorded PID's
// comm is "firecracker", which the reaper checks). It runs detached in its own session so
// the VM survives a fleet-agent restart; a goroutine reaps it when it eventually exits.
func (d *FirecrackerDriver) startJailer(id session.ID) (int, error) {
	args := []string{
		"--id", id.String(),
		"--uid", fmt.Sprintf("%d", d.cfg.JailUID),
		"--gid", fmt.Sprintf("%d", d.cfg.JailGID),
		"--chroot-base-dir", d.cfg.JailDir,
		"--exec-file", d.cfg.FirecrackerBin,
		"--", "--api-sock", "/firecracker.socket",
	}
	cmd := exec.Command(d.cfg.JailerBin, args...) //nolint:gosec // args are internal, not user input
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start jailer: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap on eventual exit; VM outlives this process's parent
	return pid, nil
}

// placeResources reflink-copies the rootfs and hardlinks the workspace and kernel into the
// chroot, then hands ownership to the jailed uid so Firecracker can open them.
func (d *FirecrackerDriver) placeResources(ctx context.Context, spec LaunchSpec, root string) error {
	rootfs := filepath.Join(root, "rootfs.ext4")
	// CoW clone when the FS supports it (XFS/Btrfs), else a full copy on ext4.
	if err := d.runner.Run(ctx, "cp", "--reflink=auto", spec.RootfsSource, rootfs); err != nil {
		return fmt.Errorf("place rootfs: %w", err)
	}
	if err := linkOrCopy(ctx, d.runner, spec.WorkspacePath, filepath.Join(root, "workspace.ext4")); err != nil {
		return fmt.Errorf("place workspace: %w", err)
	}
	if err := linkOrCopy(ctx, d.runner, spec.KernelPath, filepath.Join(root, "vmlinux")); err != nil {
		return fmt.Errorf("place kernel: %w", err)
	}
	for _, f := range []string{rootfs, filepath.Join(root, "workspace.ext4"), filepath.Join(root, "vmlinux")} {
		if err := os.Chown(f, d.cfg.JailUID, d.cfg.JailGID); err != nil {
			return fmt.Errorf("chown %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// configure drives the Firecracker API to boot the VM.
func (d *FirecrackerDriver) configure(ctx context.Context, spec LaunchSpec, sock string) error {
	client := udsClient(sock)
	bootArgs := spec.BootArgs
	if bootArgs == "" {
		bootArgs = defaultBootArgs
	}

	steps := []struct {
		path string
		body any
	}{
		{"/boot-source", map[string]any{
			"kernel_image_path": "/vmlinux",
			"boot_args":         bootArgs,
		}},
		{"/drives/rootfs", map[string]any{
			"drive_id": "rootfs", "path_on_host": "/rootfs.ext4",
			"is_root_device": true, "is_read_only": true,
		}},
		{"/drives/workspace", map[string]any{
			"drive_id": "workspace", "path_on_host": "/workspace.ext4",
			"is_root_device": false, "is_read_only": false,
		}},
		{"/network-interfaces/eth0", map[string]any{
			"iface_id": "eth0", "host_dev_name": spec.Net.Tap,
			"guest_mac": deriveMAC(spec.Net.GuestIP),
		}},
		{"/machine-config", map[string]any{
			"vcpu_count": spec.VCPUs, "mem_size_mib": spec.MemMiB, "smt": false,
		}},
		// MMDS V2 requires a session token to read, blocking casual SSRF-shaped reads (§9).
		{"/mmds/config", map[string]any{
			"version": "V2", "network_interfaces": []string{"eth0"},
			"ipv4_address": d.cfg.mmdsAddr(),
		}},
		{"/mmds", spec.MMDS},
		{"/actions", map[string]any{"action_type": "InstantStart"}},
	}
	for _, s := range steps {
		if err := udsPut(ctx, client, s.path, s.body); err != nil {
			return fmt.Errorf("configure %s: %w", s.path, err)
		}
	}
	return nil
}

// Destroy stops the VM and removes its ephemeral chroot. The persistent workspace volume is
// untouched — only its hardlink inside the chroot goes away (PLAN §7.6, I7).
func (d *FirecrackerDriver) Destroy(_ context.Context, rec VMRecord) error {
	if rec.PID > 0 {
		killProcess(rec.PID, 5*time.Second)
	}
	dir := rec.Chroot
	if dir == "" {
		dir = d.instanceDir(rec.Session)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("destroy: remove chroot: %w", err)
	}
	return nil
}

// mmdsAddr returns the MMDS link-local address (default 169.254.169.254).
func (c Config) mmdsAddr() string { return "169.254.169.254" }

// --- helpers -------------------------------------------------------------------

// deriveMAC builds a stable locally-administered MAC from an IPv4 address, e.g.
// 172.16.0.2 -> "06:00:ac:10:00:02". Deterministic and unique per guest IP.
func deriveMAC(ipv4 string) string {
	ip := net.ParseIP(ipv4).To4()
	if ip == nil {
		return "06:00:00:00:00:00"
	}
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3])
}

// linkOrCopy hardlinks src to dst (same-FS, free), falling back to a copy across
// filesystems.
func linkOrCopy(ctx context.Context, runner Runner, src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return runner.Run(ctx, "cp", "--", src, dst)
}

// waitForSocket polls for path until it exists or timeout elapses.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("api socket %s did not appear within %s", path, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// killProcess sends SIGTERM then, after grace, SIGKILL.
func killProcess(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// udsClient returns an HTTP client bound to a unix-domain socket.
func udsClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// udsPut PUTs body as JSON to the Firecracker API path over the UDS client.
func udsPut(ctx context.Context, client *http.Client, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}
