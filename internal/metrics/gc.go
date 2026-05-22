package metrics

import (
	"time"

	"github.com/kennguy3n/zk-drive/internal/gc"
)

// RecordGCRun emits all orphan-object GC metrics for a single GCAll
// invocation. Call it once per run from the scheduler (cmd/worker's
// runOrphanGC loop or the one-shot cmd/orphan-gc binary). The
// duration is computed from the supplied start time so the caller
// controls exactly which scope is timed.
//
// Result classification mirrors RecordReconcilerRun:
//
//   - err == nil               → result="ok"
//   - errors.Is(err, ctx.Canc) → result="cancelled"
//   - other err                → result="error"
//
// Per-workspace errors in the Summary are folded into the
// gcWorkspaceErrorsTotal counter even on a result="error" run, for
// the same reason the reconciler does: partial data is still useful
// for operators triaging which workspaces are stuck.
func (m *Metrics) RecordGCRun(summary gc.Summary, runErr error, start time.Time) {
	elapsed := time.Since(start).Seconds()
	m.gcRunDuration.Observe(elapsed)

	if summary.Workspaces > 0 {
		m.gcWorkspacesScanned.Add(float64(summary.Workspaces))
	}
	if summary.OrphansFound > 0 {
		m.gcOrphansFound.Add(float64(summary.OrphansFound))
	}
	if summary.OrphansDeleted > 0 {
		m.gcOrphansDeleted.Add(float64(summary.OrphansDeleted))
	}
	if summary.ObjectsDeleted > 0 {
		m.gcObjectsDeleted.Add(float64(summary.ObjectsDeleted))
	}
	if n := len(summary.Errors); n > 0 {
		m.gcWorkspaceErrorsTotal.Add(float64(n))
	}

	m.gcRunsTotal.WithLabelValues(reconcilerResultLabel(runErr)).Inc()
}
