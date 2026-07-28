// Package reconcile is the control-plane safety net that kills orphaned microVMs — VMs the
// fleet host is running that no authority (Temporal, in M4) still claims. It complements
// the host reaper (PLAN §7.7): Temporal is authoritative; the host reaper only enforces
// absolute lifetime for VMs the control plane never learned about.
package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/chelodo/fleet/internal/session"
)

// VM is the reconciler's minimal view of a running VM, decoded from fleet-agent's GET /vms.
type VM struct {
	Session   session.ID `json:"session_id"`
	StartedAt int64      `json:"started_at"`
}

// FleetClient talks to a fleet host's fleet-agent API.
type FleetClient interface {
	List(ctx context.Context) ([]VM, error)
	Destroy(ctx context.Context, id session.ID, purgeWorkspace bool) error
}

// Authority reports which sessions are still expected to be running. In M2 this is a
// file-backed set; M4 swaps in a Temporal-querying implementation without touching the loop.
type Authority interface {
	ExpectedSessions(ctx context.Context) (map[session.ID]struct{}, error)
}

// Reconciler diffs the fleet's actual VMs against the authority's expected set and destroys
// the difference.
type Reconciler struct {
	client    FleetClient
	authority Authority
	grace     time.Duration
	log       *slog.Logger
	now       func() time.Time
}

// Options configure a Reconciler.
type Options struct {
	// Grace is how long a just-launched VM is exempt from reaping, to avoid a race where a
	// VM exists on the host before the authority has recorded it.
	Grace time.Duration
	Log   *slog.Logger
	// Now is overridable in tests; defaults to time.Now.
	Now func() time.Time
}

// New returns a Reconciler.
func New(client FleetClient, authority Authority, opts Options) *Reconciler {
	if opts.Grace <= 0 {
		opts.Grace = 3 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Reconciler{client: client, authority: authority, grace: opts.Grace, log: opts.Log, now: opts.Now}
}

// ReconcileOnce performs a single pass and returns the sessions it destroyed. Orphans are
// destroyed keeping their workspace: the VM is unexpected, but a PR may still reference the
// volume, and only a deliberate DestroyWorkspace discards it (PLAN §7.6, I7).
func (r *Reconciler) ReconcileOnce(ctx context.Context) ([]session.ID, error) {
	actual, err := r.client.List(ctx)
	if err != nil {
		return nil, err
	}
	expected, err := r.authority.ExpectedSessions(ctx)
	if err != nil {
		return nil, err
	}

	now := r.now().Unix()
	var killed []session.ID
	for _, vm := range actual {
		if _, ok := expected[vm.Session]; ok {
			continue // still claimed
		}
		if age := now - vm.StartedAt; age < int64(r.grace.Seconds()) {
			r.log.Debug("skip young unclaimed vm", "session_id", vm.Session, "age_s", age)
			continue // too young — may be mid-registration
		}
		r.log.Warn("reaping orphan vm", "session_id", vm.Session)
		if err := r.client.Destroy(ctx, vm.Session, false); err != nil {
			r.log.Error("orphan destroy failed", "session_id", vm.Session, "err", err)
			continue
		}
		killed = append(killed, vm.Session)
	}
	return killed, nil
}

// Run reconciles every interval until ctx is cancelled. Errors are logged, not fatal — a
// transient fleet-agent outage must not stop the loop.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	r.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	killed, err := r.ReconcileOnce(ctx)
	if err != nil {
		r.log.Error("reconcile pass failed", "err", err)
		return
	}
	if len(killed) > 0 {
		r.log.Info("reconcile pass complete", "orphans_reaped", len(killed))
	}
}
