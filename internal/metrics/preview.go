package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// RecordPreviewBudgetExceeded increments the preview-budget-exceeded
// counter for one rejected preview job, partitioned by the owning
// workspace's billing tier. Nil-safe so the no-metrics boot mode (and
// unit tests that construct a preview service without a Metrics) pay
// only a nil-check.
//
// Implements internal/preview.BudgetObserver so the preview package
// records this counter without importing internal/metrics — the same
// metrics-implements-observer inversion used for the cache observer.
func (m *Metrics) RecordPreviewBudgetExceeded(tier string) {
	if m == nil || m.previewBudgetExceededTotal == nil {
		return
	}
	m.previewBudgetExceededTotal.WithLabelValues(tier).Inc()
}

// registerPreviewMetrics mounts the preview-pipeline counters on the
// supplied registry. Same promauto.With(reg) pattern as every other
// metric family in metrics.New().
func (m *Metrics) registerPreviewMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)

	m.previewBudgetExceededTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_preview_budget_exceeded_total",
		Help: "Total preview-generation jobs deferred because the owning workspace exhausted its per-window preview budget, partitioned by billing tier (bounded). NOT labelled by workspace_id (unbounded cardinality); use the worker's structured logs for per-workspace investigation.",
	}, []string{"tier"})
}
