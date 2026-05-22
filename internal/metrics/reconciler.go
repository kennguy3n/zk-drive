package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/kennguy3n/zk-drive/internal/reconciler"
)

// RecordReconcilerRun emits all reconciler-related metrics for a
// single ReconcileAll invocation. Call it once per run from the
// scheduler (cmd/worker's runStorageReconciler loop or the one-
// shot cmd/reconciler binary). The duration is computed from the
// supplied start time so the caller controls exactly which scope
// is timed.
//
// Result classification follows the same rule as cmd/reconciler's
// exit-code contract:
//
//   - err == nil               → result="ok"
//   - errors.Is(err, ctx.Canc) → result="cancelled" (graceful shutdown)
//   - other err                → result="error"
//
// The Summary's per-workspace error slice is folded into the
// reconciler_workspace_errors_total counter — even on a
// result="error" run we want the partial per-workspace data
// surfaced because that's the same defensive logging cmd/worker
// already does (see the loop-body comment in runStorageReconciler).
func (m *Metrics) RecordReconcilerRun(summary reconciler.Summary, runErr error, start time.Time) {
	elapsed := time.Since(start).Seconds()
	m.reconcilerRunDuration.Observe(elapsed)

	m.reconcilerWorkspacesScanned.Add(float64(summary.Workspaces))
	m.reconcilerWorkspacesUpdated.Add(float64(summary.Updated))
	if summary.TotalDriftBytes > 0 {
		m.reconcilerDriftBytes.Add(float64(summary.TotalDriftBytes))
	}
	if n := len(summary.Errors); n > 0 {
		m.reconcilerWorkspaceErrorsTotal.Add(float64(n))
	}

	m.reconcilerRunsTotal.WithLabelValues(reconcilerResultLabel(runErr)).Inc()
}

// reconcilerResultLabel maps a ReconcileAll error to one of the
// three result-label values. Kept tiny + free of allocations so
// it's safe to inline-call on the steady-state path.
func reconcilerResultLabel(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "error"
	}
}
