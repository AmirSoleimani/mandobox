// Package reconcile is the fleet-reconciliation library: it identifies orphaned microVMs — ones
// the fleet host is running that no authority (Temporal's open workflows) still claims. Temporal is
// authoritative; this package only decides which host VMs are unexpected. The control plane drives
// it from the ReconcileWorkflow (a scheduled Temporal workflow), reaping each orphan via DestroyVM.
package reconcile

import (
	"context"
	"time"

	"github.com/AmirSoleimani/mandobox/internal/session"
)

// VM is the minimal view of a running VM, decoded from mando-agent's GET /vms.
type VM struct {
	Session   session.ID `json:"session_id"`
	StartedAt int64      `json:"started_at"` // epoch seconds
}

// Authority reports which sessions are still expected to be running. TemporalAuthority backs it
// with Temporal's Running PRWorkflows; an interface so callers can inject a fake in tests.
type Authority interface {
	ExpectedSessions(ctx context.Context) (map[session.ID]struct{}, error)
}

// FindOrphans returns the sessions whose VMs are unexpected (no live workflow) and older than the
// grace period. Grace exempts a just-launched VM whose workflow hasn't yet surfaced in Temporal's
// visibility (eventual consistency), so it isn't reaped mid-registration. Reaping keeps the
// workspace volume (a PR may still reference it) — that is the caller's DestroyVM choice; this
// only identifies the orphans.
func FindOrphans(actual []VM, expected map[session.ID]struct{}, grace time.Duration, now time.Time) []session.ID {
	nowUnix := now.Unix()
	var orphans []session.ID
	for _, vm := range actual {
		if _, ok := expected[vm.Session]; ok {
			continue // still claimed by a live workflow
		}
		if nowUnix-vm.StartedAt < int64(grace.Seconds()) {
			continue // too young — may be mid-registration
		}
		orphans = append(orphans, vm.Session)
	}
	return orphans
}
