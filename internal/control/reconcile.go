package control

import (
	"context"
	"fmt"
	"time"

	"github.com/acme/mandobox/internal/reconcile"
	"github.com/acme/mandobox/internal/session"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The orphan-VM reaper runs as a scheduled Temporal workflow on the worker (it replaced the
// standalone fleet-reconciler service). Temporal is authoritative for what should be running; this
// just reaps host VMs whose workflow has ended (Terminate skips a workflow's own teardown, a lost
// LaunchVM ack can orphan a VM the workflow never learned about, a host reboot desyncs state, etc.).
// Reconciliation is inherently GLOBAL (all host VMs vs all live workflows), so it can't live inside
// a per-session workflow — hence its own scheduled workflow.
const (
	// ReconcileScheduleID is the Temporal schedule firing ReconcileWorkflow. Stable, so the worker
	// creates-if-absent on startup (idempotent across restarts).
	ReconcileScheduleID = "fleet-reconcile"
	reconcileWorkflowID = "fleet-reconcile-run"
	// ReconcileInterval is how often the reaper runs (parity with the old standalone reconciler).
	ReconcileInterval = 5 * time.Minute
	// defaultReconcileGrace exempts a just-launched VM whose workflow hasn't surfaced in visibility.
	defaultReconcileGrace = 3 * time.Minute
)

// OrphanResult is the FindOrphanVMs activity result: session IDs whose VMs have no live workflow.
type OrphanResult struct {
	Orphans []string `json:"orphans"`
}

// ReconcileResult is the ReconcileWorkflow result: the sessions whose VMs it reaped this pass.
type ReconcileResult struct {
	Reaped []string `json:"reaped"`
}

// FindOrphanVMs lists the host's actual VMs and Temporal's expected (Running) sessions, returning
// the VMs with no live workflow that are older than the grace period. It FAILS CLOSED: if either
// the host list or the Temporal query errors, it returns the error so the pass reaps nothing rather
// than everything.
func (a *Activities) FindOrphanVMs(ctx context.Context) (OrphanResult, error) {
	vms, err := a.Fleet.List(ctx)
	if err != nil {
		return OrphanResult{}, fmt.Errorf("list vms: %w", err)
	}
	expected, err := a.ReconcileAuthority.ExpectedSessions(ctx)
	if err != nil {
		return OrphanResult{}, fmt.Errorf("expected sessions: %w", err)
	}
	actual := make([]reconcile.VM, 0, len(vms))
	for _, v := range vms {
		actual = append(actual, reconcile.VM{Session: session.ID(v.Session), StartedAt: v.StartedAt})
	}
	grace := a.ReconcileGrace
	if grace <= 0 {
		grace = defaultReconcileGrace
	}
	var out OrphanResult
	for _, id := range reconcile.FindOrphans(actual, expected, grace, time.Now()) {
		out.Orphans = append(out.Orphans, id.String())
	}
	return out, nil
}

// ReconcileWorkflow is the scheduled reaper. Each run finds orphan VMs and destroys each via the
// same DestroyVM activity the PRWorkflow uses (keeping the workspace volume). Running on the
// worker, it inherits Temporal's durability, retries, and visibility — every pass and every reap is
// in Temporal history, and a failed pass surfaces there rather than in a separate service's logs.
func ReconcileWorkflow(ctx workflow.Context) (ReconcileResult, error) {
	var a *Activities
	log := workflow.GetLogger(ctx)

	findCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	var found OrphanResult
	if err := workflow.ExecuteActivity(findCtx, a.FindOrphanVMs).Get(ctx, &found); err != nil {
		// Fail-closed: we couldn't establish the expected set, so we reap nothing. Surfaced as a
		// failed run for visibility; the next scheduled pass retries.
		return ReconcileResult{}, fmt.Errorf("find orphans: %w", err)
	}

	destroyCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 120 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	})
	var res ReconcileResult
	for _, sid := range found.Orphans {
		// Keep the workspace volume (a PR may still reference it); only the VM is unexpected.
		if err := workflow.ExecuteActivity(destroyCtx, a.DestroyVM, DestroyParams{SessionID: sid}).Get(ctx, nil); err != nil {
			log.Error("reconcile: reap failed", "session_id", sid, "err", err)
			continue
		}
		res.Reaped = append(res.Reaped, sid)
	}
	if len(res.Reaped) > 0 {
		log.Info("reconcile: reaped orphan vms", "count", len(res.Reaped), "sessions", res.Reaped)
	}
	return res, nil
}

// EnsureReconcileSchedule creates the reconcile schedule if it doesn't already exist (idempotent on
// worker restart). Overlap defaults to SKIP, so a slow pass never stacks up.
func EnsureReconcileSchedule(ctx context.Context, c client.Client, interval time.Duration) error {
	if _, err := c.ScheduleClient().GetHandle(ctx, ReconcileScheduleID).Describe(ctx); err == nil {
		return nil // already exists
	}
	_, err := c.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID:   ReconcileScheduleID,
		Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{Every: interval}}},
		Action: &client.ScheduleWorkflowAction{
			ID:        reconcileWorkflowID,
			Workflow:  ReconcileWorkflow,
			TaskQueue: TaskQueue,
		},
	})
	return err
}
