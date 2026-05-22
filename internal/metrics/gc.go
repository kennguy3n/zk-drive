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

	// Unconditional Add() on the loop-counter families: matches
	// RecordReconcilerRun in this package — prometheus.Counter.Add(0)
	// is a documented no-op, so a `> 0` guard here would only diverge
	// from the reconciler's pattern without changing behaviour. The
	// only guarded family is Errors, mirrored from the reconciler,
	// where the guard avoids the len() conversion when the run was
	// clean.
	m.gcWorkspacesScanned.Add(float64(summary.Workspaces))
	m.gcOrphansFound.Add(float64(summary.OrphansFound))
	m.gcOrphansDeleted.Add(float64(summary.OrphansDeleted))
	m.gcObjectsDeleted.Add(float64(summary.ObjectsDeleted))
	if n := len(summary.Errors); n > 0 {
		m.gcWorkspaceErrorsTotal.Add(float64(n))
	}

	m.gcRunsTotal.WithLabelValues(reconcilerResultLabel(runErr)).Inc()
}
