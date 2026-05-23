package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// AuditArchiveResultOK / Partial / Error / Cancelled are the bounded
// label values for auditArchiveRunsTotal. Exported so the archiver
// binary can reference them without re-typing the string literals
// (a typo would mint a new series that nobody alerts on).
const (
	AuditArchiveResultOK        = "ok"
	AuditArchiveResultPartial   = "partial"
	AuditArchiveResultError     = "error"
	AuditArchiveResultCancelled = "cancelled"
)

// AuditArchiveBucketResultOK / Error are the bucket-level label
// values for auditArchiveBucketsTotal. Bucket here means one
// (workspace, year-month) tuple.
const (
	AuditArchiveBucketResultOK    = "ok"
	AuditArchiveBucketResultError = "error"
)

// RecordAuditArchiveRun emits the per-invocation run counter and
// the duration histogram. Called once at the end of each
// audit-archiver run regardless of outcome so the dashboard always
// records the cadence even when the run errored.
func (m *Metrics) RecordAuditArchiveRun(result string, durationSeconds float64) {
	m.auditArchiveRunsTotal.WithLabelValues(result).Inc()
	m.auditArchiveRunDuration.Observe(durationSeconds)
}

// RecordAuditArchiveBucket emits the per-(workspace, month) bucket
// counter, the cumulative rows-archived counter, and the cumulative
// bytes-uploaded counter. Called once per successful bucket. On
// failure the caller passes 0 for rows and bytes so the counters
// don't go up — divergence between buckets and rows is a real
// signal of partial-failure ingestion.
func (m *Metrics) RecordAuditArchiveBucket(result string, rows int, bytes int64) {
	m.auditArchiveBucketsTotal.WithLabelValues(result).Inc()
	if rows > 0 {
		m.auditArchiveRowsTotal.Add(float64(rows))
	}
	if bytes > 0 {
		m.auditArchiveBytesTotal.Add(float64(bytes))
	}
}

// registerAuditArchiveMetrics mounts the audit-archive metric
// family on the supplied registry. Same promauto.With(reg) shape as
// every other metric family in metrics.New() so contributors copying
// the pattern can't accidentally double-register.
func (m *Metrics) registerAuditArchiveMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)

	m.auditArchiveRunsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_audit_archive_runs_total",
		Help: "Total invocations of the audit-log archiver, partitioned by result ('ok' = all buckets archived; 'partial' = some bucket(s) failed and were left in the hot tier for the next run; 'error' = run aborted before bucket iteration; 'cancelled' = SIGTERM mid-run).",
	}, []string{"result"})

	m.auditArchiveRowsTotal = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_audit_archive_rows_total",
		Help: "Cumulative count of audit_log rows successfully moved from the hot tier to S3 cold archive across all archiver runs.",
	})

	m.auditArchiveBytesTotal = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_audit_archive_bytes_total",
		Help: "Cumulative uncompressed JSONL bytes uploaded to S3 cold archive. Plot alongside zkdrive_audit_archive_rows_total to track per-row size trends.",
	})

	m.auditArchiveBucketsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_audit_archive_buckets_total",
		Help: "Total (workspace, year-month) buckets the archiver attempted, partitioned by result ('ok' = upload+delete+record committed; 'error' = upload, delete, or record failed; rows still in hot tier for next run). NOT labelled by workspace_id (unbounded cardinality).",
	}, []string{"result"})

	m.auditArchiveRunDuration = auto.NewHistogram(prometheus.HistogramOpts{
		Name:    "zkdrive_audit_archive_run_duration_seconds",
		Help:    "Wall time per audit-archiver invocation. Useful for validating the K8s CronJob completes within its scheduled cadence.",
		Buckets: auditArchiveRunBuckets,
	})
}

// auditArchiveRunBuckets covers the audit-archiver runtime range.
// Lowest bucket (10 ms) catches the no-work case (zero workspaces
// with rows older than retention); highest bucket (4 hours) caps
// at the default CronJob activeDeadlineSeconds. A run that exceeds
// the cadence (typically nightly) is the operational signal that
// audit volume is outpacing per-run throughput and the cadence
// needs to be tightened OR MaxRowsPerBatch raised.
var auditArchiveRunBuckets = []float64{
	0.01, 0.1, 1, 5, 10, 30, 60, 300, 600, 1800, 3600, 14400,
}
