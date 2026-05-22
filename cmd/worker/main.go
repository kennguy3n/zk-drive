// zk-drive worker binary — Phase 2.
//
// The worker hosts JetStream consumers for the drive.* subjects the
// API server publishes to after a successful upload:
//
//   drive.preview.generate — image thumbnail (Go stdlib + x/image)
//   drive.scan.virus       — ClamAV virus scan over INSTREAM
//   drive.search.index     — Postgres FTS index refresh (placeholder)
//
// Each handler resolves its dependencies at startup (Postgres pool,
// zk-object-fabric storage client, optional ClamAV address) and runs
// inline against the enqueued file_id / version_id tuple. Job results
// (preview rows, scan verdicts) are persisted back to Postgres so the
// server can surface them without talking to NATS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/kennguy3n/zk-drive/internal/classify"
	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/index"
	"github.com/kennguy3n/zk-drive/internal/jobs"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/metrics"
	"github.com/kennguy3n/zk-drive/internal/notification"
	"github.com/kennguy3n/zk-drive/internal/permission"
	"github.com/kennguy3n/zk-drive/internal/preview"
	"github.com/kennguy3n/zk-drive/internal/reconciler"
	"github.com/kennguy3n/zk-drive/internal/retention"
	"github.com/kennguy3n/zk-drive/internal/scan"
	"github.com/kennguy3n/zk-drive/internal/sharing"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/version"
	"github.com/kennguy3n/zk-drive/internal/wiring"
)

const (
	streamName  = "DRIVE_JOBS"
	defaultNATS = "nats://localhost:4222"
	ackWait     = 5 * time.Minute
)

func main() {
	if err := run(); err != nil {
		slog.Error("worker exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("worker")
	slog.Info("zk-drive worker starting", "version", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		return fmt.Errorf("connect postgres: %w", err)
	}
	// Defer order matters — defers run LIFO, so the target shutdown
	// sequence (relative to each other and to pool.Close) is:
	//
	//   1. cancel()          — signals long-running goroutines to
	//                          stop (runGuestExpirySweep,
	//                          runStorageReconciler).
	//   2. bgGoroutines.Wait — blocks until those goroutines have
	//                          observed ctx.Done() and returned, so
	//                          no caller is mid-Acquire on the pool.
	//   3. pool.Close()      — closes the pool against a quiescent
	//                          set of consumers; no "use of closed
	//                          connection" log noise.
	//
	// We register them in the reverse of that target order. The
	// NATS defers (nc.Drain, unsubscribeAll) added later run before
	// cancel() in LIFO order; that is intentional because NATS
	// Drain processes any in-flight message callbacks (which may
	// hold pool conns) before we signal the rest of the goroutines
	// to exit.
	var bgGoroutines sync.WaitGroup
	defer pool.Close()
	defer bgGoroutines.Wait()
	defer cancel()

	// Same precondition as cmd/server: migrations are owned by the
	// dedicated migrate binary now, not run inline on worker
	// startup. Failing fast here ensures a worker doesn't begin
	// consuming jobs against a stale schema (which would emit
	// cryptic "column does not exist" errors for every job).
	if err := database.RequireMinMigrationVersion(ctx, pool); err != nil {
		return fmt.Errorf("startup precondition: %w", err)
	}

	// Storage client is optional: if the worker is started without
	// S3_ENDPOINT it can only log incoming jobs (same placeholder
	// behaviour as before). In production the server and worker share
	// the same S3 configuration.
	var storageClient *storage.Client
	if cfg.S3Endpoint != "" {
		storageClient, err = storage.NewClient(storage.Config{
			Endpoint:  cfg.S3Endpoint,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		})
		if err != nil {
			return fmt.Errorf("storage client: %w", err)
		}
		slog.Info("worker storage client wired", "endpoint", cfg.S3Endpoint, "bucket", cfg.S3Bucket)
	} else {
		slog.Info("worker S3_ENDPOINT unset; preview/scan jobs will be logged only")
	}

	notifSvc := notification.NewService(notification.NewPostgresRepository(pool))

	var previewSvc *preview.Service
	var scanSvc *scan.Service
	var archiveSvc *retention.ArchiveService
	var indexSvc *index.Service
	if storageClient != nil {
		previewSvc = preview.NewService(pool, storageClient, preview.NewPostgresRepository(pool))
		scanSvc = scan.NewService(pool, storageClient, os.Getenv("CLAMAV_ADDRESS"))
		scanSvc.SetNotifier(notifSvc)
		archiveSvc = retention.NewArchiveService(pool, storageClient, nil)
		indexSvc = index.NewService(pool, storageClient, nil)
	}

	// Classification reads nothing from object storage — name + mime
	// are enough — so it is wired unconditionally. Strict-ZK folders
	// still short-circuit inside the handler.
	classifySvc := classify.NewService(pool)

	// Guest expiry sweep runs on a timer inside the worker binary so
	// the server process doesn't take on extra cron-like
	// responsibilities. A 5-minute cadence is fine for Phase 3 —
	// share-link TTLs are generally hours / days.
	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), wiring.NewPermissionGranter(permission.NewService(permission.NewPostgresRepository(pool))))
	bgGoroutines.Add(1)
	go func() {
		defer bgGoroutines.Done()
		runGuestExpirySweep(ctx, sharingSvc, 5*time.Minute)
	}()

	// Storage-counter reconciliation (WS-14). Runs inside the worker
	// process on a configurable cadence so the denormalized
	// workspaces.storage_used_bytes column converges back to the
	// canonical SUM(files.size_bytes) over time, even if a future
	// code path forgets to update it. Default cadence is 60m;
	// RECONCILE_INTERVAL_MINUTES=0 disables the in-process loop
	// (deploys that prefer a dedicated K8s CronJob set it to 0 and
	// schedule /app/reconciler externally).
	// metrics owns a private prometheus.Registry plus the pool
	// collectors. The worker's HTTP /metrics surface is a tiny
	// dedicated server on cfg.WorkerMetricsAddr (default :9091)
	// because the worker binary is otherwise headless — see
	// startMetricsServer for the contract. metricsSurface is
	// passed into both the reconciler loop and the NATS
	// subscribers so worker_jobs_total / reconciler_* land on
	// the same registry the HTTP server scrapes.
	metricsSurface := metrics.New()
	metricsSurface.RegisterPgxPoolCollector(pool)

	if interval := reconcileInterval(); interval > 0 {
		bgGoroutines.Add(1)
		rc := reconciler.New(pool)
		go func() {
			defer bgGoroutines.Done()
			runStorageReconciler(ctx, rc, metricsSurface, interval)
		}()
	}

	// Start the metrics HTTP server before NATS so a scraper
	// observing /healthz on :9091 can confirm the worker is alive
	// even during the (typically sub-second) NATS connect step.
	// The returned shutdown closure runs after sigCh fires so
	// in-flight scrape requests can drain cleanly.
	shutdownMetrics, err := startMetricsServer(ctx, cfg.WorkerMetricsAddr, metricsSurface, &bgGoroutines)
	if err != nil {
		return fmt.Errorf("metrics server: %w", err)
	}
	defer shutdownMetrics()

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = defaultNATS
	}

	nc, err := nats.Connect(natsURL,
		nats.Name("zk-drive-worker"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", natsURL, err)
	}
	defer nc.Drain() //nolint:errcheck // best-effort drain during shutdown

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	if err := ensureStream(js); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}

	subs, err := subscribeAll(ctx, js, pool, metricsSurface, previewSvc, scanSvc, archiveSvc, indexSvc, classifySvc)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer unsubscribeAll(subs)

	slog.Info("zk-drive worker listening", "nats_url", natsURL, "stream", streamName)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	return nil
}

// startMetricsServer brings up a tiny HTTP server on listenAddr
// exposing /metrics + /healthz so an operator can scrape the
// worker the same way they scrape the main API server. The
// returned shutdown closure does a graceful 5-second drain on
// any in-flight scrape requests when the worker exits.
//
// listenAddr == "" or "off" disables the server entirely (no
// listen, no goroutine started, shutdown closure is a no-op).
// That's the escape hatch for deployments that use a different
// metrics collection path (e.g. statsd sidecar) or that don't
// want a second listening port on the worker pod.
//
// The /healthz handler here is the worker-side equivalent of
// the server's /healthz: a shallow "process is alive" check
// that never pings downstream dependencies. There is
// intentionally no /readyz here because the worker has no
// dispatchable traffic to gate — NATS handles its own
// consumer re-balancing if the worker becomes unhealthy.
func startMetricsServer(_ context.Context, listenAddr string, m *metrics.Metrics, wg *sync.WaitGroup) (func(), error) {
	listenAddr = strings.TrimSpace(listenAddr)
	if listenAddr == "" || strings.EqualFold(listenAddr, "off") {
		slog.Info("worker metrics server disabled (WORKER_METRICS_ADDR empty or 'off')")
		return func() {}, nil
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","component":"worker","version":"` + version.Version + `"}`))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("worker metrics server listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("worker metrics server exited", "err", err)
		}
	}()

	shutdown := func() {
		// Independent timeout so a stuck scraper can't pin
		// shutdown past the outer process-exit budget. Five
		// seconds is enough to flush any in-flight scrape
		// response (which is single-digit ms in practice).
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Error("worker metrics server shutdown failed", "err", err)
		}
	}
	return shutdown, nil
}

// ensureStream creates or updates the DRIVE_JOBS stream that backs
// every drive.* subject. Running AddStream with an existing name
// returns ErrStreamNameAlreadyInUse; we fall through to UpdateStream
// so stream config stays current across deploys without manual
// migration.
func ensureStream(js nats.JetStreamContext) error {
	cfg := &nats.StreamConfig{
		Name:      streamName,
		Subjects:  []string{jobs.SubjectPreview, jobs.SubjectScan, jobs.SubjectIndex, jobs.SubjectArchive, jobs.SubjectRetention, jobs.SubjectClassify},
		Storage:   nats.FileStorage,
		Retention: nats.WorkQueuePolicy,
		MaxAge:    7 * 24 * time.Hour,
	}
	if _, err := js.AddStream(cfg); err != nil {
		if errors.Is(err, nats.ErrStreamNameAlreadyInUse) {
			if _, uerr := js.UpdateStream(cfg); uerr != nil {
				return uerr
			}
			return nil
		}
		return err
	}
	return nil
}

// subscribeAll wires a durable consumer for each subject. Durable
// names let the worker restart without losing checkpoint state.
// Each metrics.JobHandler is wrapped via InstrumentJob so the
// (subject, result) labels land on zkdrive_worker_jobs_total and
// the duration histogram captures wall time per subject.
func subscribeAll(ctx context.Context, js nats.JetStreamContext, pool *pgxpool.Pool, m *metrics.Metrics, previewSvc *preview.Service, scanSvc *scan.Service, archiveSvc *retention.ArchiveService, indexSvc *index.Service, classifySvc *classify.Service) ([]*nats.Subscription, error) {
	subjects := []struct {
		subject string
		durable string
		handler nats.MsgHandler
	}{
		{jobs.SubjectPreview, "drive-preview", m.InstrumentJob(jobs.SubjectPreview, previewHandler(ctx, pool, previewSvc))},
		{jobs.SubjectScan, "drive-scan", m.InstrumentJob(jobs.SubjectScan, scanHandler(ctx, pool, scanSvc))},
		{jobs.SubjectIndex, "drive-index", m.InstrumentJob(jobs.SubjectIndex, indexHandler(ctx, pool, indexSvc))},
		{jobs.SubjectArchive, "drive-archive", m.InstrumentJob(jobs.SubjectArchive, archiveHandler(ctx, archiveSvc))},
		{jobs.SubjectClassify, "drive-classify", m.InstrumentJob(jobs.SubjectClassify, classifyHandler(ctx, pool, classifySvc))},
	}
	var subs []*nats.Subscription
	for _, s := range subjects {
		sub, err := js.Subscribe(s.subject, s.handler,
			nats.Durable(s.durable),
			nats.AckWait(ackWait),
			nats.DeliverAll(),
			nats.ManualAck(),
		)
		if err != nil {
			unsubscribeAll(subs)
			return nil, fmt.Errorf("subscribe %s: %w", s.subject, err)
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

func unsubscribeAll(subs []*nats.Subscription) {
	for _, s := range subs {
		_ = s.Unsubscribe()
	}
}

// previewHandler decodes the FileJob envelope and runs the preview
// service. Unsupported mime types (ErrUnsupportedMime) ack and
// return JobResult "skip" because the file is simply not previewable;
// every other failure Nak's ("error") so NATS redelivers on the next
// AckWait cycle. Returning the JobResult lets metrics.InstrumentJob
// emit the right (subject, result) label tuple — see
// internal/metrics/worker.go for the contract.
func previewHandler(ctx context.Context, pool *pgxpool.Pool, svc *preview.Service) metrics.JobHandler {
	return func(msg *nats.Msg) metrics.JobResult {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			slog.Error("worker malformed preview payload", "err", err)
			_ = msg.Term()
			return metrics.JobResultDropped
		}
		if isStrictZK(ctx, pool, job.FileID) {
			slog.Info("worker skipping strict-zk file (preview)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		if svc == nil {
			slog.Warn("worker preview skipped (no storage client)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		p, err := svc.Generate(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			if errors.Is(err, preview.ErrUnsupportedMime) {
				slog.Info("worker preview unsupported mime", "file_id", job.FileID, "version_id", job.VersionID)
				_ = msg.Ack()
				return metrics.JobResultSkip
			}
			slog.Error("worker preview failed", "file_id", job.FileID, "version_id", job.VersionID, "err", err)
			_ = msg.Nak()
			return metrics.JobResultError
		}
		slog.Info("worker preview ok", "file_id", job.FileID, "version_id", job.VersionID, "key", p.ObjectKey)
		_ = msg.Ack()
		return metrics.JobResultOK
	}
}

// scanHandler decodes the FileJob envelope and runs the scan service.
// Successful verdicts (clean / quarantined) are acked; transient
// failures (pending — typically clamd connectivity errors) are Nak'd
// so NATS redelivers on the next AckWait cycle. The final status is
// persisted to file_versions so operators can audit results via SQL.
func scanHandler(ctx context.Context, pool *pgxpool.Pool, svc *scan.Service) metrics.JobHandler {
	return func(msg *nats.Msg) metrics.JobResult {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			slog.Error("worker malformed scan payload", "err", err)
			_ = msg.Term()
			return metrics.JobResultDropped
		}
		if isStrictZK(ctx, pool, job.FileID) {
			slog.Info("worker skipping strict-zk file (scan)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		if svc == nil {
			slog.Warn("worker scan skipped (no storage client)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		v, err := svc.Scan(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			slog.Error("worker scan error", "file_id", job.FileID, "version_id", job.VersionID, "err", err)
			_ = msg.Nak()
			return metrics.JobResultError
		}
		slog.Info("worker scan complete", "status", v.Status, "file_id", job.FileID, "version_id", job.VersionID, "detail", v.Detail)
		_ = msg.Ack()
		return metrics.JobResultOK
	}
}

// archiveHandler compresses and uploads a single version's bytes to
// the cold archive key pattern, then stamps archived_at on the row.
// Missing storage client -> ack and move on (the same pattern as
// preview/scan).
func archiveHandler(ctx context.Context, svc *retention.ArchiveService) metrics.JobHandler {
	return func(msg *nats.Msg) metrics.JobResult {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			slog.Error("worker malformed archive payload", "err", err)
			_ = msg.Term()
			return metrics.JobResultDropped
		}
		if svc == nil {
			slog.Warn("worker archive skipped (no storage client)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := svc.ArchiveVersion(jobCtx, job.VersionID); err != nil {
			slog.Error("worker archive failed", "file_id", job.FileID, "version_id", job.VersionID, "err", err)
			_ = msg.Nak()
			return metrics.JobResultError
		}
		slog.Info("worker archive ok", "file_id", job.FileID, "version_id", job.VersionID)
		_ = msg.Ack()
		return metrics.JobResultOK
	}
}

// runGuestExpirySweep periodically revokes expired guest permission
// rows. The first sweep runs 30 seconds after startup so the worker
// doesn't race the server's migration pass on cold start. The
// initial delay is a cancellable select rather than time.Sleep so
// SIGTERM during the 30-second warm-up returns immediately — the
// goroutine is now WaitGroup-tracked (bgGoroutines.Wait() blocks
// pool teardown on it returning), and a plain time.Sleep would
// delay graceful shutdown by up to 30s.
func runGuestExpirySweep(ctx context.Context, svc *sharing.Service, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		revoked, err := svc.ExpireGuestAccess(ctx, time.Now().UTC())
		if err != nil {
			slog.Error("worker guest expiry sweep failed", "err", err)
		} else if revoked > 0 {
			slog.Info("worker guest expiry sweep revoked permissions", "revoked", revoked)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcileInterval reads RECONCILE_INTERVAL_MINUTES and returns the
// cadence for the in-process storage counter reconciler. Returns 0
// only when the env var parses to a zero integer ("0", "00", "+0",
// etc.) — the documented operator opt-out for deploys where a
// dedicated K8s CronJob owns reconciliation. The default (used when
// the env var is unset, an empty string, or fails to parse) is 60
// minutes: short enough that frontend reads converge to the
// canonical sum within an hour of any drift event, long enough that
// the periodic SELECT/UPDATE pair is well below the noise floor of
// regular database traffic. Falling back to the default on parse
// failure (rather than disabling) is the conservative choice: a
// typo'd value silently disabling a safety-net reconciler is the
// worse outcome, and duplicate runs against a dedicated CronJob are
// idempotent (row-level lock + no-op UPDATE when there's no drift).
func reconcileInterval() time.Duration {
	raw := os.Getenv("RECONCILE_INTERVAL_MINUTES")
	if raw == "" {
		return 60 * time.Minute
	}
	mins, err := strconv.Atoi(raw)
	if err != nil || mins < 0 {
		slog.Warn("worker invalid RECONCILE_INTERVAL_MINUTES; defaulting to 60", "raw", raw)
		return 60 * time.Minute
	}
	return time.Duration(mins) * time.Minute
}

// runStorageReconciler drives reconciler.ReconcileAll on a fixed
// cadence. The first run is delayed 60s after startup so the worker
// doesn't fight a fresh server's connection-pool warmup, then it
// fires every `interval`. Errors are logged but do not stop the
// loop — a single bad SQL execution shouldn't wedge the worker.
//
// Cadence note: this uses time.NewTicker, so the loop body counts
// towards the next tick. If ReconcileAll ever exceeds `interval`
// (would require ~thousands of workspaces × many ms each), the
// ticker will already have fired and the next iteration starts
// immediately — runs become back-to-back rather than at-most-once-
// per-interval. Acceptable here because reconciliation is
// idempotent (FOR UPDATE row lock + no-op UPDATE on no drift) and
// the loop body is synchronous so there's never more than one
// concurrent run. WS-17's reconciler_runtime_seconds metric is the
// signal for "reconcile is now slower than the configured cadence,
// time to shard or relax the interval".
func runStorageReconciler(ctx context.Context, rc *reconciler.Reconciler, m *metrics.Metrics, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(60 * time.Second):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := time.Now()
		summary, err := rc.ReconcileAll(ctx)
		m.RecordReconcilerRun(summary, err, start)
		// Surface per-workspace errors + run summary regardless of
		// the function-level err, mirroring cmd/reconciler. When a
		// transient enumeration failure aborts a run with err !=
		// nil but the loop still has 50 workspaces of partial data,
		// dropping that data on the floor makes the next on-caller
		// reach for `kubectl logs --previous` instead of the
		// current run's output. The context.Canceled branch stays
		// quiet (shutdown is expected, and the partial summary on
		// shutdown is rarely actionable) but every other err path
		// gets the full summary.
		for _, e := range summary.Errors {
			slog.Error("worker reconciler per-workspace failure", "workspace_id", e.WorkspaceID, "err", e.Err)
		}
		if err == nil || !errors.Is(err, context.Canceled) {
			slog.Info("worker reconciler ran",
				"workspaces", summary.Workspaces,
				"updated", summary.Updated,
				"drift_bytes", summary.TotalDriftBytes,
				"errors", len(summary.Errors),
				"duration", time.Since(start).Round(time.Millisecond).String(),
			)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("worker storage reconciler aborted", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// indexHandler downloads the uploaded object, extracts text, and
// writes it to files.content_text so the search FTS query in
// internal/search can score on body content. Strict-ZK files short-
// circuit before any download — the server's plaintext never leaves
// the device for those folders by design.
//
// When the worker boots without a storage client the handler falls
// back to the original logging-only behaviour so cold-start setups
// (no S3_ENDPOINT) still drain the subject and don't queue up
// JetStream messages.
func indexHandler(ctx context.Context, pool *pgxpool.Pool, svc *index.Service) metrics.JobHandler {
	return func(msg *nats.Msg) metrics.JobResult {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			slog.Error("worker malformed index payload", "err", err)
			_ = msg.Term()
			return metrics.JobResultDropped
		}
		if isStrictZK(ctx, pool, job.FileID) {
			slog.Info("worker skipping strict-zk file (index)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		if svc == nil {
			slog.Warn("worker index acked (no storage)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := svc.IndexFile(jobCtx, job.FileID, job.VersionID); err != nil {
			slog.Error("worker index failed", "file_id", job.FileID, "version_id", job.VersionID, "err", err)
			_ = msg.Nak()
			return metrics.JobResultError
		}
		slog.Info("worker index ok", "file_id", job.FileID, "version_id", job.VersionID)
		_ = msg.Ack()
		return metrics.JobResultOK
	}
}

// classifyHandler decodes the FileJob envelope and runs the
// classification service. Strict-ZK files skip + ack so the server
// never writes a label derived from plaintext it does not hold.
func classifyHandler(ctx context.Context, pool *pgxpool.Pool, svc *classify.Service) metrics.JobHandler {
	return func(msg *nats.Msg) metrics.JobResult {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			slog.Error("worker malformed classify payload", "err", err)
			_ = msg.Term()
			return metrics.JobResultDropped
		}
		if isStrictZK(ctx, pool, job.FileID) {
			slog.Info("worker skipping strict-zk file (classify)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		if svc == nil {
			slog.Warn("worker classify acked (no pool)", "file_id", job.FileID, "version_id", job.VersionID)
			_ = msg.Ack()
			return metrics.JobResultSkip
		}
		jobCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		if err := svc.Classify(jobCtx, job.FileID); err != nil {
			slog.Error("worker classify failed", "file_id", job.FileID, "version_id", job.VersionID, "err", err)
			_ = msg.Nak()
			return metrics.JobResultError
		}
		slog.Info("worker classify ok", "file_id", job.FileID, "version_id", job.VersionID)
		_ = msg.Ack()
		return metrics.JobResultOK
	}
}

// isStrictZK looks up the encryption_mode of the folder owning the
// file. Errors are logged and treated as managed-encrypted so the
// worker fails open (continues processing) instead of silently
// stalling the pipeline. The actual cross-mode invariant is enforced
// at the API layer; this is the worker-side optimisation that keeps
// strict-zk objects out of preview / scan / index codepaths.
func isStrictZK(ctx context.Context, pool *pgxpool.Pool, fileID uuid.UUID) bool {
	if pool == nil {
		return false
	}
	mode, err := folder.EncryptionModeForFile(ctx, pool, fileID)
	if err != nil {
		slog.Error("worker lookup encryption mode failed", "file_id", fileID, "err", err)
		return false
	}
	return mode == folder.EncryptionStrictZK
}
