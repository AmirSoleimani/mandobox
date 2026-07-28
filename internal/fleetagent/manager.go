package fleetagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chelodo/fleet/internal/session"
)

// ErrAtCapacity is returned when the fleet is already running MaxVMs. The HTTP layer maps
// it to 503 so the LaunchVM activity retries with backoff (PLAN §7.1, EX_TEMPFAIL).
var ErrAtCapacity = errors.New("fleet at capacity")

// ErrForbiddenMMDS is returned when a launch payload would place a Tier-0 credential in a
// guest (invariant I9). fleet-agent refuses rather than trusting the caller.
var ErrForbiddenMMDS = errors.New("mmds payload contains a forbidden key")

// procAlive reports whether a pid names a live process. It is a package var so tests can
// substitute a deterministic implementation.
var procAlive = func(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

// nowUnix returns the current epoch seconds; overridable in tests.
var nowUnix = func() int64 { return time.Now().Unix() }

const (
	defaultVCPUs  = 2
	defaultMemMiB = 4096
)

// LaunchRequest is a validated launch, assembled from the API request.
type LaunchRequest struct {
	Session          session.ID
	ImageSHA         string
	VCPUs            int
	MemMiB           int
	BootArgs         string
	WorkspaceSizeMiB int
	MMDS             map[string]any
}

// Manager owns the VM lifecycle: it prepares the rootfs, workspace, and network, drives the
// VMDriver, and records state. A single mutex serialises allocation and capacity so two
// concurrent launches never claim the same /30 or overshoot MaxVMs.
type Manager struct {
	cfg    Config
	store  *StateStore
	ws     *Workspace
	net    *Network
	driver VMDriver
	log    *slog.Logger

	mu sync.Mutex
}

// NewManager wires a Manager. runner backs workspace/network host commands; driver boots
// the VMs (a FirecrackerDriver in production, a fake in tests).
func NewManager(cfg Config, runner Runner, driver VMDriver, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		cfg:    cfg,
		store:  NewStateStore(cfg.RunStateDir),
		ws:     NewWorkspace(cfg, runner),
		net:    NewNetwork(cfg, runner),
		driver: driver,
		log:    log,
	}
}

// Launch boots a VM for req.Session, or returns its existing record if one is already
// running (idempotent under retry). Returns ErrAtCapacity when the fleet is full.
func (m *Manager) Launch(ctx context.Context, req LaunchRequest) (VMRecord, error) {
	if !req.Session.Valid() {
		return VMRecord{}, fmt.Errorf("launch: invalid session_id %q", req.Session)
	}
	if forbiddenMMDSKey(req.MMDS) {
		return VMRecord{}, ErrForbiddenMMDS
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Idempotency / stale cleanup.
	if rec, err := m.store.Get(req.Session); err == nil {
		if procAlive(rec.PID) {
			m.log.Info("launch: session already running", "session_id", req.Session, "pid", rec.PID)
			return rec, nil
		}
		m.log.Warn("launch: clearing dead record before relaunch", "session_id", req.Session, "pid", rec.PID)
		_ = m.driver.Destroy(ctx, rec)
		_ = m.net.DeleteTap(ctx, rec.Tap)
		_ = m.store.Delete(req.Session)
	}

	list, err := m.store.List()
	if err != nil {
		return VMRecord{}, fmt.Errorf("launch: list: %w", err)
	}
	if len(list) >= m.cfg.MaxVMs {
		return VMRecord{}, ErrAtCapacity
	}

	rootfsSrc, err := m.ws.EnsureRootfs(ctx, req.ImageSHA)
	if err != nil {
		return VMRecord{}, fmt.Errorf("launch: rootfs: %w", err)
	}
	wsPath, _, err := m.ws.Ensure(ctx, req.Session, req.WorkspaceSizeMiB)
	if err != nil {
		return VMRecord{}, fmt.Errorf("launch: workspace: %w", err)
	}
	gnet, err := m.net.Allocate(req.Session, list)
	if err != nil {
		return VMRecord{}, fmt.Errorf("launch: allocate: %w", err)
	}
	if err := m.net.CreateTap(ctx, gnet); err != nil {
		return VMRecord{}, fmt.Errorf("launch: tap: %w", err)
	}

	spec := LaunchSpec{
		Session:       req.Session,
		KernelPath:    m.cfg.KernelPath,
		RootfsSource:  rootfsSrc,
		WorkspacePath: wsPath,
		Net:           gnet,
		VCPUs:         orDefault(req.VCPUs, defaultVCPUs),
		MemMiB:        orDefault(req.MemMiB, defaultMemMiB),
		BootArgs:      req.BootArgs,
		MMDS:          mergeMMDS(req.MMDS, req.Session, gnet),
	}
	res, err := m.driver.Launch(ctx, spec)
	if err != nil {
		_ = m.net.DeleteTap(ctx, gnet.Tap) // driver self-cleans its process/chroot on failure
		return VMRecord{}, fmt.Errorf("launch: driver: %w", err)
	}

	rec := VMRecord{
		Session:   req.Session,
		ImageSHA:  req.ImageSHA,
		Tap:       gnet.Tap,
		Chroot:    res.Chroot,
		Workspace: wsPath,
		GuestIP:   gnet.GuestIP,
		HostIP:    gnet.HostIP,
		VCPUs:     spec.VCPUs,
		MemMiB:    spec.MemMiB,
		PID:       res.PID,
		StartedAt: nowUnix(),
	}
	if err := m.store.Put(rec); err != nil {
		// State write failed after boot: tear the VM down rather than leak an untracked VM (I8).
		_ = m.driver.Destroy(ctx, rec)
		_ = m.net.DeleteTap(ctx, gnet.Tap)
		return VMRecord{}, fmt.Errorf("launch: record state: %w", err)
	}
	m.log.Info("launched", "session_id", req.Session, "pid", rec.PID, "tap", rec.Tap, "guest_ip", rec.GuestIP)
	return rec, nil
}

// Destroy stops a VM and frees its tap and runtime state. The workspace volume is preserved
// unless purgeWorkspace is set (PLAN §6.1: DestroyVM keeps it; DestroyWorkspace discards).
// It is idempotent: destroying an unknown session is not an error.
func (m *Manager) Destroy(ctx context.Context, id session.ID, purgeWorkspace bool) error {
	if !id.Valid() {
		return fmt.Errorf("destroy: invalid session_id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, err := m.store.Get(id)
	switch {
	case errors.Is(err, ErrNotFound):
		// No live VM. Still honour a workspace purge (DestroyWorkspace after DestroyVM).
		if purgeWorkspace {
			return m.ws.Destroy(ctx, id)
		}
		return nil
	case err != nil:
		return fmt.Errorf("destroy: get: %w", err)
	}

	if err := m.driver.Destroy(ctx, rec); err != nil {
		m.log.Error("destroy: driver", "session_id", id, "err", err)
	}
	if err := m.net.DeleteTap(ctx, rec.Tap); err != nil {
		m.log.Warn("destroy: delete tap", "session_id", id, "tap", rec.Tap, "err", err)
	}
	if err := m.store.Delete(id); err != nil {
		return fmt.Errorf("destroy: delete state: %w", err)
	}
	if purgeWorkspace {
		if err := m.ws.Destroy(ctx, id); err != nil {
			return fmt.Errorf("destroy: workspace: %w", err)
		}
	}
	m.log.Info("destroyed", "session_id", id, "purge_workspace", purgeWorkspace)
	return nil
}

// List returns every VM fleet-agent is tracking, for reconciliation (PLAN §7.7).
func (m *Manager) List() ([]VMRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store.List()
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// mergeMMDS returns a shallow copy of payload augmented with the network facts fleet-agent
// allocated at launch (the guest learns its own IP from MMDS — §8.1) and the session_id for
// correlation. fleet-agent's network is authoritative and overrides any provided value.
func mergeMMDS(payload map[string]any, id session.ID, g GuestNet) map[string]any {
	out := make(map[string]any, len(payload)+2)
	maps.Copy(out, payload)
	out["session_id"] = id.String()
	out["network"] = g
	return out
}

// forbiddenMMDSKey reports whether payload contains, at any depth, a key that must never
// reach a guest (invariant I9). This is a defensive guard; the primary enforcement is the
// guest environment (M3).
func forbiddenMMDSKey(payload map[string]any) bool {
	const forbidden = "anthropic_api_key"
	var walk func(any) bool
	walk = func(v any) bool {
		m, ok := v.(map[string]any)
		if !ok {
			return false
		}
		for k, child := range m {
			if strings.EqualFold(strings.TrimSpace(k), forbidden) {
				return true
			}
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(payload)
}
