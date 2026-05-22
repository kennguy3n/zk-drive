// Package metrics is the process-wide Prometheus surface for ZK
// Drive. It owns a private prometheus.Registry, the set of
// application-level metric vectors (HTTP, worker jobs, reconciler
// runs), and the pool-stat collectors that translate runtime
// connection-pool state into gauges on each scrape.
//
// Why a private registry (instead of prometheus.DefaultRegisterer)
//
// Tests can construct as many independent Metrics values as they
// like without colliding on the global registry. Worker, server,
// and reconciler binaries each get their own Metrics, so a metric
// name registered once in cmd/server doesn't accidentally double-
// register when cmd/worker boots in the same address space (e.g.
// via integration tests that link both binaries' packages).
//
// Convention for new metrics
//
//   - All names live under the "zkdrive_" prefix so a single
//     Prometheus job scraping a multi-tenant cluster can grep this
//     project's series without ambiguity.
//   - Histograms expose _seconds suffix on duration; counters
//     expose _total; gauges expose the bare units. Matches the
//     prometheus naming guide.
//   - Labels are bounded sets only. NEVER label by user_id /
//     workspace_id / request_id / object_key / URL path — those
//     are high-cardinality and will blow up the scrape store. Use
//     the chi RoutePattern (which is bounded by the number of
//     registered routes) for HTTP labels and the constant NATS
//     subject string for worker labels.
//
// WS-17 ships the core surface; subsequent workstreams can add
// histograms (e.g. preview generation latency by mime type) by
// constructing them via promauto.With(m.Registry).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics is the binding between the application code paths
// (HTTP middleware, worker job wrapper, reconciler hook) and a
// single isolated prometheus.Registry. Construct one per process
// and pass it down.
//
// All fields are intentionally typed as the *Vec interfaces from
// client_golang so consumers can stub them in tests without
// reaching into prometheus internals.
type Metrics struct {
	// Registry is the Prometheus registry that promhttp.Handler
	// scrapes. Exposed so callers (e.g. cmd/server) can mount the
	// /metrics endpoint and tests can call Gather() directly.
	Registry *prometheus.Registry

	// httpRequestsTotal counts every HTTP request the server
	// handled. Labels: method (GET/POST/PUT/DELETE/...), route
	// (chi RoutePattern, e.g. "/api/files/{fileID}"), status
	// (decimal HTTP status). RoutePattern bounds cardinality to
	// the registered route set; the unmatched case is labelled
	// "not_matched" so 404 storms cannot mint new series.
	httpRequestsTotal *prometheus.CounterVec

	// httpRequestDuration observes server-side wall time per
	// request. Labels: method, route (no status — adding status
	// would double cardinality and we already separate failures
	// via httpRequestsTotal). Buckets cover the realistic SME
	// API latency range (sub-millisecond presigned-URL mint
	// through ~10s archive-fetch); see newHTTPHistogram for
	// rationale.
	httpRequestDuration *prometheus.HistogramVec

	// httpInFlightRequests gauges the number of HTTP requests
	// currently being served. Useful for spotting goroutine
	// pile-ups (e.g. a slow downstream stalling every handler)
	// before they manifest as p99 latency spikes.
	httpInFlightRequests prometheus.Gauge

	// workerJobsTotal counts NATS JetStream jobs processed by
	// the worker binary. Labels: subject (drive.preview /
	// drive.scan / etc.), result ("ok" / "skip" / "error" /
	// "dropped"). See JobResult for the result classification
	// contract.
	workerJobsTotal *prometheus.CounterVec

	// workerJobDuration observes wall time per worker job.
	// Labels: subject only — result is intentionally absent
	// because long-running successes and short-failing errors
	// have very different distributions and mixing them obscures
	// both signals. Operators can plot p99 success latency by
	// joining workerJobsTotal{result="ok"} on this histogram.
	workerJobDuration *prometheus.HistogramVec

	// reconcilerRunsTotal counts top-level reconciler.ReconcileAll
	// invocations. Labels: result ("ok" / "error" / "cancelled").
	// One scrape series per result; useful for a steady-state
	// alert (if result=error rate > 0 over 24h, page someone).
	reconcilerRunsTotal *prometheus.CounterVec

	// reconcilerWorkspacesScanned counts the number of workspace
	// rows the reconciler inspected. No labels — the per-result
	// breakdown lives on reconcilerRunsTotal and per-workspace
	// failures are surfaced via reconcilerWorkspaceErrorsTotal.
	reconcilerWorkspacesScanned prometheus.Counter

	// reconcilerWorkspacesUpdated counts how many workspaces
	// needed a counter rewrite (stored != canonical sum). In a
	// healthy production system this should be near-zero in
	// steady state; a rising value is the signal that an
	// upload/delete code path is drifting from the schema
	// invariant.
	reconcilerWorkspacesUpdated prometheus.Counter

	// reconcilerDriftBytes accumulates the absolute |new - old|
	// difference across every workspace updated. Operators can
	// compute "drift gigabytes per hour" via rate() to alert on
	// systemic drift events vs. one-off cosmetic corrections.
	reconcilerDriftBytes prometheus.Counter

	// reconcilerWorkspaceErrorsTotal counts per-workspace failures
	// inside a run. Note: NOT labelled by workspace_id — that
	// would be unbounded cardinality. Operators chase per-
	// workspace failures via the structured worker logs which
	// DO carry workspace_id; this metric is the high-level
	// "how many workspaces are in trouble right now" signal.
	reconcilerWorkspaceErrorsTotal prometheus.Counter

	// reconcilerRunDuration observes wall time per ReconcileAll
	// invocation. No labels. Used to validate the
	// runStorageReconciler comment in cmd/worker that warns when
	// reconciliation slows below the configured cadence.
	reconcilerRunDuration prometheus.Histogram

	// gcRunsTotal counts top-level GCService.GCAll invocations,
	// partitioned by the same result classifier as the
	// reconciler ('ok', 'error', 'cancelled'). The shared label
	// vocabulary lets operators reuse the existing reconciler
	// alert rules verbatim for orphan-object GC.
	gcRunsTotal *prometheus.CounterVec

	// gcWorkspacesScanned counts how many workspace rows the GC
	// loop inspected. Identical semantics to the reconciler's
	// reconcilerWorkspacesScanned counter (no labels; per-error
	// breakdown lives on gcWorkspaceErrorsTotal).
	gcWorkspacesScanned prometheus.Counter

	// gcOrphansFound counts every orphan file row the scan returned
	// across all GC runs. Diverges from gcOrphansDeleted only when a
	// concurrent ConfirmUpload races the predicate-guarded DELETE
	// (benign) or when the per-workspace storage delete fails AND
	// the row delete also fails (rare; surfaces as gcRunsTotal
	// result=error).
	gcOrphansFound prometheus.Counter

	// gcOrphansDeleted counts orphan file rows the GC reconciler
	// successfully reclaimed via DeletePendingOrphan. The gap
	// (gcOrphansFound - gcOrphansDeleted) is the count of confirm
	// races and per-row delete errors combined.
	gcOrphansDeleted prometheus.Counter

	// gcObjectsDeleted counts S3 objects the GC reconciler
	// successfully removed via storage.DeleteObject. Less than
	// gcOrphansDeleted is normal (storage unconfigured for a
	// suspended workspace, or DeleteObject returned a transient
	// gateway error which the row-delete path tolerates by design).
	gcObjectsDeleted prometheus.Counter

	// gcWorkspaceErrorsTotal counts per-workspace failures inside
	// GC runs. Same cardinality discipline as the reconciler
	// counterpart (no workspace_id label).
	gcWorkspaceErrorsTotal prometheus.Counter

	// gcRunDuration observes wall time per GCAll invocation.
	// Tracked separately from the reconciler histogram because the
	// shape is different: GC runs are dominated by per-orphan
	// DeleteObject latency (network round-trip per row), whereas
	// reconciler runs are dominated by per-workspace SUM
	// (single SQL aggregate per workspace).
	gcRunDuration prometheus.Histogram
}

// New constructs a Metrics with a fresh private registry and the
// default Go runtime + process collectors registered. The runtime
// collector emits goroutine count / heap usage / gc latency; the
// process collector emits open FDs, CPU time, RSS — all standard
// signals operators need but client_golang does not bind by default
// when using a custom registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m := &Metrics{Registry: reg}

	auto := promauto.With(reg)

	m.httpRequestsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_http_requests_total",
		Help: "Total HTTP requests handled by the API server, partitioned by method, chi route pattern, and decimal status code. Unmatched paths use route='not_matched'.",
	}, []string{"method", "route", "status"})

	m.httpRequestDuration = auto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zkdrive_http_request_duration_seconds",
		Help:    "Server-side wall time per HTTP request, partitioned by method and chi route pattern.",
		Buckets: httpDurationBuckets,
	}, []string{"method", "route"})

	m.httpInFlightRequests = auto.NewGauge(prometheus.GaugeOpts{
		Name: "zkdrive_http_in_flight_requests",
		Help: "Number of HTTP requests currently being served by the API server.",
	})

	m.workerJobsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_worker_jobs_total",
		Help: "Total NATS JetStream jobs processed by the worker, partitioned by subject and result ('ok' = processed, 'skip' = strict-zk or no service configured, 'error' = transient failure (Nak), 'dropped' = poison payload (Term)).",
	}, []string{"subject", "result"})

	m.workerJobDuration = auto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "zkdrive_worker_job_duration_seconds",
		Help:    "Wall time per NATS JetStream job, partitioned by subject. Includes both success and failure paths; join with zkdrive_worker_jobs_total for per-result breakdowns.",
		Buckets: jobDurationBuckets,
	}, []string{"subject"})

	m.reconcilerRunsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_reconciler_runs_total",
		Help: "Total invocations of the storage-counter reconciler's ReconcileAll, partitioned by result ('ok', 'error', 'cancelled').",
	}, []string{"result"})

	m.reconcilerWorkspacesScanned = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_reconciler_workspaces_scanned_total",
		Help: "Cumulative number of workspace rows the reconciler inspected across all runs.",
	})

	m.reconcilerWorkspacesUpdated = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_reconciler_workspaces_updated_total",
		Help: "Cumulative number of workspace rows whose storage_used_bytes counter the reconciler had to rewrite (drift was non-zero).",
	})

	m.reconcilerDriftBytes = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_reconciler_drift_bytes_total",
		Help: "Cumulative absolute |canonical - stored| drift bytes the reconciler corrected. rate() to alert on systemic drift events.",
	})

	m.reconcilerWorkspaceErrorsTotal = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_reconciler_workspace_errors_total",
		Help: "Cumulative count of per-workspace failures inside reconciler runs. NOT labelled by workspace_id (unbounded cardinality); use structured logs for per-workspace investigation.",
	})

	m.reconcilerRunDuration = auto.NewHistogram(prometheus.HistogramOpts{
		Name:    "zkdrive_reconciler_run_duration_seconds",
		Help:    "Wall time per ReconcileAll invocation. Useful for validating that reconcile completes within the configured cadence.",
		Buckets: reconcilerRunBuckets,
	})

	m.gcRunsTotal = auto.NewCounterVec(prometheus.CounterOpts{
		Name: "zkdrive_gc_runs_total",
		Help: "Total invocations of the orphan-object GC's GCAll, partitioned by result ('ok', 'error', 'cancelled'). Same label vocabulary as the reconciler so alert rules can be parameterised on the prefix.",
	}, []string{"result"})

	m.gcWorkspacesScanned = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_gc_workspaces_scanned_total",
		Help: "Cumulative number of workspace rows the orphan GC inspected across all runs.",
	})

	m.gcOrphansFound = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_gc_orphans_found_total",
		Help: "Cumulative number of orphan presigned uploads the GC scan returned (pre-delete).",
	})

	m.gcOrphansDeleted = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_gc_orphans_deleted_total",
		Help: "Cumulative number of orphan file rows the GC successfully reclaimed.",
	})

	m.gcObjectsDeleted = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_gc_objects_deleted_total",
		Help: "Cumulative number of S3 objects the GC successfully removed via storage.DeleteObject. Diverges from gcOrphansDeleted when the per-workspace storage path is unconfigured or transiently failing.",
	})

	m.gcWorkspaceErrorsTotal = auto.NewCounter(prometheus.CounterOpts{
		Name: "zkdrive_gc_workspace_errors_total",
		Help: "Cumulative count of per-workspace failures inside GC runs. NOT labelled by workspace_id (unbounded cardinality); use structured logs for per-workspace investigation.",
	})

	m.gcRunDuration = auto.NewHistogram(prometheus.HistogramOpts{
		Name:    "zkdrive_gc_run_duration_seconds",
		Help:    "Wall time per GCAll invocation. Tracked separately from the reconciler histogram because GC runs are dominated by per-orphan DeleteObject latency.",
		Buckets: gcRunBuckets,
	})

	return m
}

// httpDurationBuckets covers the realistic SME API latency range
// for ZK Drive. Lowest bucket (1 ms) catches presigned-URL mints
// and other pure-Postgres lookups; highest bucket (10 s) catches
// the slowest legitimate path (a cold archive fetch behind a
// cross-region S3 round-trip). Bucket count is 11 — a healthy
// trade-off between resolution and per-series storage cost in
// Prometheus.
var httpDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 10,
}

// jobDurationBuckets covers the worker job runtime range.
// Lowest bucket (10 ms) catches the strict-ZK skip path
// (single SELECT + ack) and the no-service skip path. Highest
// bucket (10 min) caps at the archiveHandler's per-message
// timeout — anything beyond that is a runaway and will be
// killed by the context deadline anyway.
var jobDurationBuckets = []float64{
	0.01, 0.05, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300, 600,
}

// reconcilerRunBuckets covers the ReconcileAll runtime range.
// Lowest bucket (10 ms) catches an empty-workspace deploy where
// the reconciler scans zero rows; highest bucket (1 hour) caps
// at the default cadence — if a run takes longer than the
// cadence itself, the runStorageReconciler comment in
// cmd/worker is the signal to shard or relax the interval.
var reconcilerRunBuckets = []float64{
	0.01, 0.1, 1, 5, 10, 30, 60, 300, 600, 1800, 3600,
}

// gcRunBuckets covers the GCAll runtime range. Lowest bucket (10 ms)
// catches the steady-state path where every workspace has zero
// orphans (one index-only SELECT per workspace, nothing to delete);
// highest bucket (6 hours) caps at the default GC cadence — a run
// that exceeds the cadence is the operational signal that orphans
// are arriving faster than the GC can reclaim them and the
// per-workspace limit needs to be raised or the cadence shortened.
var gcRunBuckets = []float64{
	0.01, 0.1, 1, 5, 10, 30, 60, 300, 600, 1800, 3600, 21600,
}
