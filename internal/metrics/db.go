package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DB operation labels. Closed set of strings — adding a new query
// family means adding a constant here, NOT free-form labels at the
// call site. The label vocabulary is intentionally tied to the
// repository method name (`{package}.{method_snake_case}`) so an
// operator reading the Prometheus surface can map every series back
// to a single Go function without grep gymnastics.
const (
	// DBOpPermissionCheckAccess covers the flat (non-inheriting)
	// access check on internal/permission.PostgresRepository.
	DBOpPermissionCheckAccess = "permission.check_access"
	// DBOpPermissionCheckAccessWithInheritance covers the
	// ancestor-walking access check that dominates the
	// per-request DB cost without caching. Tracking it
	// separately from DBOpPermissionCheckAccess lets operators
	// measure the cache's effective hit ratio (the cached path
	// short-circuits before this op is recorded).
	DBOpPermissionCheckAccessWithInheritance = "permission.check_access_with_inheritance"
	// DBOpFileListInFolder covers the hot folder-browse query
	// in internal/file.PostgresRepository.ListInFolder.
	DBOpFileListInFolder = "file.list_in_folder"
	// DBOpFolderListChildren covers the hot folder-browse query
	// in internal/folder.PostgresRepository.ListChildren and
	// ListRootChildren.
	DBOpFolderListChildren = "folder.list_children"
	// DBOpChangeLogSince covers the cursor-pagination query
	// served to every desktop sync client on reconnect. Should
	// stay well under p99 100ms; if it climbs, the
	// idx_change_log_workspace_sequence composite index is the
	// first thing to suspect.
	DBOpChangeLogSince = "change_log.since"
)

// DB result labels. Bounded to a 3-element set so dashboards can
// alert on `result=error` rate without worrying about a typo
// minting a new series. NotFound is deliberately separate from
// Error because pgx.ErrNoRows is an *expected* outcome for many
// lookups (e.g. "does this user have an explicit grant on this
// folder?" — usually no) and conflating it with error would mask
// real DB failures.
const (
	DBResultOK       = "ok"
	DBResultError    = "error"
	DBResultNotFound = "not_found"
)

// dbQueryDurationBuckets covers the realistic per-query latency
// range for SME workloads against a healthy Postgres pool: lowest
// bucket (100µs) catches a cached PK lookup; highest bucket (5s)
// catches a degraded query plan or pool exhaustion before the
// statement-timeout fires. Bucket count matches httpDurationBuckets
// for visual consistency on dashboards.
var dbQueryDurationBuckets = []float64{
	0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// RecordDBQuery emits the per-query observability histogram +
// counter. Called from inside a repository method via a deferred
// closure that captures the start time and named-return error so
// every exit path (success, error, early return) is observed
// uniformly. duration is the wall-time the operation took; result
// is one of the DBResult* constants above.
//
// Implements the abstract observer surface that callsites depend
// on (e.g. internal/permission.DBObserver) so packages can record
// metrics without importing this package directly (avoids a cycle
// when cmd/server wires the metrics surface).
//
// Nil-safe: callers receive (*Metrics)(nil) when running in a
// boot mode that doesn't install metrics (e.g. cmd/migrate, the
// audit-restore CLI). Recording on a nil receiver is a no-op so
// the cost of instrumentation in those binaries is one
// nil-check and no allocation.
func (m *Metrics) RecordDBQuery(op string, duration time.Duration, result string) {
	if m == nil || m.dbQueryDuration == nil {
		return
	}
	m.dbQueriesTotal.WithLabelValues(op, result).Inc()
	m.dbQueryDuration.WithLabelValues(op).Observe(duration.Seconds())
}

// DBObserver is the minimal surface a repository depends on to
// emit DB query metrics. Defined here so consumers can spell out
// the dependency without importing the full metrics package — and
// so unit tests can supply a recording fake without the full
// promauto registry plumbing.
type DBObserver interface {
	RecordDBQuery(op string, duration time.Duration, result string)
}

// registerDBMetrics mounts the per-query histogram + counter on
// the supplied registry. Same promauto.With(reg) pattern as every
// other metric family in metrics.New().
func (m *Metrics) registerDBMetrics(reg prometheus.Registerer) {
	auto := promauto.With(reg)

	m.dbQueriesTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_db_queries_total",
		Help: "Total Postgres queries issued by hot-path repositories, partitioned by op (one label per query family — see internal/metrics/db.go DBOp* constants) and result ('ok' = query returned successfully; 'error' = pgx returned an error other than ErrNoRows; 'not_found' = pgx.ErrNoRows, separated from error so dashboards don't false-positive on expected absences).",
	}, []string{"op", "result"})

	m.dbQueryDuration = auto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zkdrive_db_query_duration_seconds",
		Help:    "Server-observed wall time per Postgres query, partitioned by op label. Use to spot which query family dominates request latency. Result is NOT a label (errors and successes are typically the same cost, and splitting them doubles cardinality without operational benefit — join with zkdrive_db_queries_total for per-result rates).",
		Buckets: dbQueryDurationBuckets,
	}, []string{"op"})
}
