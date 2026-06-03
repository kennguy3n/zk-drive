// zk-drive worker binary.
//
// The worker hosts JetStream consumers for the drive.* subjects the
// API server publishes to after a successful upload:
//
//	drive.preview.generate — image thumbnail (Go stdlib + x/image)
//	drive.scan.virus       — ClamAV virus scan over INSTREAM
//	drive.search.index     — Postgres FTS index refresh (placeholder)
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
	"net"
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
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/zk-drive/internal/classify"
	"github.com/kennguy3n/zk-drive/internal/config"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/database"
	driveFile "github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/folder"
	"github.com/kennguy3n/zk-drive/internal/gc"
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
	"github.com/kennguy3n/zk-drive/internal/tracing"
	"github.com/kennguy3n/zk-drive/internal/version"
	"github.com/kennguy3n/zk-drive/internal/webhooks"
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

	// Tracing is initialised before any other subsystem so spans
	// from database.Connect, the NATS subscribe, and per-message
	// handlers flow through the same provider. Mirrors the
	// cmd/server startup contract: missing OTEL_EXPORTER_OTLP_ENDPOINT
	// short-circuits to a no-op provider that LogStartup announces
	// at boot. Service name is the same "zk-drive" so the worker
	// and server land under one logical service; service.instance.id
	// distinguishes the two processes.
	traceProvider, err := tracing.Init(ctx, tracing.BuildFromOperatorConfig(tracing.OperatorConfig{
		Endpoint:              cfg.OTELExporterOTLPEndpoint,
		Headers:               cfg.OTELExporterOTLPHeaders,
		Insecure:              cfg.OTELExporterOTLPInsecure,
		Compression:           cfg.OTELExporterOTLPCompression,
		ServiceName:           cfg.OTELServiceName,
		DeploymentEnvironment: cfg.OTELDeploymentEnvironment,
		SamplerRatio:          cfg.OTELSamplerRatio,
	}, version.Version))
	if err != nil {
		cancel()
		return fmt.Errorf("init tracing: %w", err)
	}
	traceProvider.LogStartup(ctx)
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		if err := traceProvider.Shutdown(shutCtx); err != nil {
			slog.Warn("tracing shutdown returned error", "err", err)
		}
	}()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		cancel()
		return fmt.Errorf("connect postgres: %w", err)
	}
	// Defer order matters — defers run LIFO, so the target shutdown
	// sequence (relative to each other and to pool.Close) is:
	//
	//   1. unsubscribeAll    — stops NATS delivering new messages
	//                          to the worker (registered later, runs
	//                          first in LIFO).
	//   2. pool drains       — for pooled subjects (priority/standard
	//                          previews) block until in-flight h(msg)
	//                          calls finish + ack while NATS is still
	//                          connected. Registered just before
	//                          unsubscribeAll so it runs right after.
	//   3. nc.Drain          — flushes in-flight message callbacks
	//                          (which may still hold pool conns and
	//                          emit reconciler / job metrics).
	//   4. shutdownMetrics   — graceful 5s drain of /metrics +
	//                          /healthz scrape requests, then
	//                          srv.Shutdown causes the metrics
	//                          server goroutine to exit and call
	//                          wg.Done on bgGoroutines.
	//   5. cancel()          — signals the remaining long-running
	//                          goroutines to stop
	//                          (runGuestExpirySweep,
	//                          runStorageReconciler).
	//   6. bgGoroutines.Wait — blocks until ALL tracked goroutines
	//                          (metrics server, reconciler, guest
	//                          sweep, pool workers) have returned, so
	//                          no caller is mid-Acquire on the pool.
	//   7. pool.Close()      — closes the pool against a quiescent
	//                          set of consumers; no "use of closed
	//                          connection" log noise.
	//   8. redisClient.Close — last: every budget/tier-cache caller
	//                          has stopped by now (see decl below).
	//
	// We register them in the reverse of that target order. The
	// NATS-related (unsubscribeAll, nc.Drain) and metrics-related
	// (shutdownMetrics) defers are registered later in this
	// function — by virtue of LIFO they run BEFORE cancel(), which
	// is intentional: NATS Drain processes in-flight message
	// callbacks (which may hold pool conns AND emit metrics) so
	// the metrics surface must still be up when those callbacks
	// run. Final scrape post-drain captures the just-completed
	// worker_jobs_total deltas before the metrics server itself
	// shuts down.
	var bgGoroutines sync.WaitGroup
	// redisClient backs the preview budget enforcer and tier cache,
	// both of which are exercised from inside NATS message handlers
	// (the pooled preview goroutines). Its Close is registered here,
	// alongside the other teardown defers, rather than inline at the
	// connect site so LIFO runs it AFTER nc.Drain + the pool drains +
	// cancel + bgGoroutines.Wait — i.e. only once every handler that
	// could touch Redis has stopped. Closing it inline (LIFO-first)
	// would tear Redis down while pool goroutines were still calling
	// Allow()/tier lookups, producing spurious "redis: client is
	// closed" shutdown noise and uncounted admissions. nil-guarded
	// because redisClient stays nil when REDIS_URL is unset or the
	// ping fails (fail-open).
	var redisClient *redis.Client
	defer func() {
		if redisClient != nil {
			_ = redisClient.Close()
		}
	}()
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

	// Redis-backed per-tenant preview budget + tier cache. Redis is
	// optional: when REDIS_URL is unset (or the ping fails) the
	// budget enforcer is nil and Generate admits every preview, so
	// single-replica / local deploys behave exactly as before. The
	// budget is the cross-replica fairness guard that stops one
	// tenant's bulk import from starving the shared worker fleet.
	// redisClient is declared up top so its Close defer can be
	// ordered with the other teardown defers (see there); here we
	// only assign it on a successful connect.
	if cfg.RedisURL != "" {
		opts, perr := redis.ParseURL(cfg.RedisURL)
		if perr != nil {
			return fmt.Errorf("parse REDIS_URL: %w", perr)
		}
		rc := redis.NewClient(opts)
		if perr := rc.Ping(ctx).Err(); perr != nil {
			slog.Warn("worker redis ping failed; preview budget disabled", "url", cfg.RedisURL, "err", perr)
			_ = rc.Close()
		} else {
			redisClient = rc
			slog.Info("worker redis connected; preview budget + tier cache enabled", "url", cfg.RedisURL)
		}
	}
	if previewSvc != nil && redisClient != nil {
		budget := preview.NewTenantPreviewBudget(redisClient, cfg.PreviewBudgetPerWorkspaceHour, preview.DefaultBudgetWindow)
		previewSvc.SetBudget(budget)
		previewSvc.SetTierCache(preview.NewTierCache(pool, redisClient, preview.DefaultTierCacheTTL))
	}

	// Classification reads nothing from object storage — name + mime
	// are enough — so it is wired unconditionally. Strict-ZK folders
	// still short-circuit inside the handler.
	classifySvc := classify.NewService(pool)

	// Guest expiry sweep runs on a timer inside the worker binary so
	// the server process doesn't take on extra cron-like
	// responsibilities. A 5-minute cadence is fine here:
	// share-link TTLs are generally hours / days.
	sharingSvc := sharing.NewService(sharing.NewPostgresRepository(pool), wiring.NewPermissionGranter(permission.NewService(permission.NewPostgresRepository(pool))))
	bgGoroutines.Add(1)
	go func() {
		defer bgGoroutines.Done()
		runGuestExpirySweep(ctx, sharingSvc, 5*time.Minute)
	}()

	// Storage-counter reconciliation. Runs inside the worker
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

	// Route preview budget-exceeded rejections onto the worker's
	// metrics registry (zkdrive_preview_budget_exceeded_total). Wired
	// here, after metricsSurface exists, so the counter lands on the
	// same registry the worker's /metrics endpoint scrapes.
	if previewSvc != nil {
		previewSvc.SetBudgetObserver(metricsSurface)
	}

	if interval := reconcileInterval(); interval > 0 {
		bgGoroutines.Add(1)
		rc := reconciler.New(pool)
		go func() {
			defer bgGoroutines.Done()
			runStorageReconciler(ctx, rc, metricsSurface, interval)
		}()
	}

	// Orphan-object GC. Reclaims S3 objects whose presigned
	// PUT completed but whose ConfirmUpload never landed. Uses the
	// same per-workspace storage resolution as the API server so
	// per-tenant zk-object-fabric buckets are correctly targeted
	// (otherwise a delete against the shared fallback client would
	// silently 404 for objects living in per-workspace tenants).
	// Default cadence is 6 hours; GC_INTERVAL_MINUTES=0 disables
	// the in-process loop (deploys preferring a dedicated K8s
	// CronJob set it to 0 and schedule /app/orphan-gc externally).
	if interval := gcInterval(); interval > 0 {
		credentialCodec, err := cryptopkg.LoadFromEnv()
		if err != nil {
			return fmt.Errorf("credential codec for GC: %w", err)
		}
		storageFactory := storage.NewClientFactory(pool, storageClient, credentialCodec)
		fileRepo := driveFile.NewPostgresRepository(pool)
		fileSvc := driveFile.NewService(fileRepo)
		gcSvc := gc.New(pool, fileSvc, workerStorageResolver(storageFactory), gc.WithTTL(gcPendingUploadTTL()))
		bgGoroutines.Add(1)
		go func() {
			defer bgGoroutines.Done()
			runOrphanGC(ctx, gcSvc, metricsSurface, interval)
		}()
		slog.Info("worker orphan-object GC enabled", "interval", interval.String(), "ttl", gcPendingUploadTTL().String())
	} else {
		slog.Info("worker orphan-object GC disabled (GC_INTERVAL_MINUTES=0)")
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

	if err := reconcilePreviewConsumers(js); err != nil {
		return fmt.Errorf("reconcile preview consumers: %w", err)
	}

	subs, poolDrains, err := subscribeAll(ctx, &bgGoroutines, js, pool, metricsSurface, previewSvc, scanSvc, archiveSvc, indexSvc, classifySvc, cfg.PreviewPriorityWorkers, cfg.PreviewStandardWorkers)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	// Registered before unsubscribeAll so LIFO runs them in the
	// order: unsubscribeAll (stop new deliveries) → pool drains
	// (finish + ack in-flight renders) → nc.Drain (close conn,
	// registered earliest, runs last). Draining before the
	// connection closes is what keeps a job that was mid-render at
	// shutdown from being redelivered.
	defer func() {
		for _, drain := range poolDrains {
			drain()
		}
	}()
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
		// Use json.NewEncoder rather than string concatenation
		// so the response is guaranteed valid JSON even if a
		// future -ldflags injection embeds quotes / control
		// characters in version.Version. Mirrors the server's
		// /healthz handler in cmd/server/main.go.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"component": "worker",
			"version":   version.Version,
		})
	})

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: mux,
		// ReadHeaderTimeout caps the slowloris window for headers.
		// ReadTimeout / WriteTimeout / IdleTimeout follow the same
		// shape as cmd/server's main HTTP server — defence-in-depth
		// against slow-read / slow-write DoS in case the metrics
		// port is ever exposed externally despite the docker-compose
		// 127.0.0.1 binding and the README's firewall guidance. A
		// well-behaved Prometheus scraper completes in well under a
		// second; these ceilings only fire on misbehaviour.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Bind synchronously here (rather than letting srv.ListenAndServe()
	// do it inside the goroutine) so a bind failure — port already in
	// use, permission denied on a privileged port, IPv6 address unable
	// to bind, etc. — surfaces as a startup error the worker can fail
	// fast on. The previous "log-and-return-nil" pattern silently left
	// the worker running without metrics; in K8s the readinessProbe
	// caught it, but in Docker Compose (and any non-k8s deployment) the
	// failure was invisible past the log line. Operator explicitly
	// enabled WORKER_METRICS_ADDR, so bind failure here is a hard
	// configuration error, not something to paper over.
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("worker metrics server bind on %s: %w", listenAddr, err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		slog.Info("worker metrics server listening", "addr", ln.Addr().String())
		// srv.Serve(ln) consumes the listener; on Shutdown it returns
		// http.ErrServerClosed. Any other error here is an unexpected
		// runtime failure (e.g. Accept syscall error) — log loudly but
		// don't crash the worker, since NATS processing is the
		// primary workload and shouldn't be tied to scrape availability.
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
		Subjects:  []string{jobs.SubjectPreview, jobs.SubjectPreviewPriority, jobs.SubjectPreviewStandard, jobs.SubjectScan, jobs.SubjectIndex, jobs.SubjectArchive, jobs.SubjectRetention, jobs.SubjectClassify, webhooks.SubjectEvents},
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

// reconcilePreviewConsumers brings the persisted MaxDeliver of the
// preview durables up to preview.QueueMaxDeliver. js.Subscribe binds
// to an existing durable without rewriting its server-side config, so
// the legacy drive-preview consumer — created by an earlier deploy
// when it still used preview.MaxDeliver (5) — would otherwise keep
// that stale cap. Because previewHandler now NakWithDelay's
// budget-deferred jobs on every preview subject and the API still
// publishes all previews to the legacy subject, a 5-delivery cap
// would terminate an over-budget workspace's previews after only a
// few minutes instead of deferring them until its window drains. We
// mirror ensureStream's reconcile-on-startup approach (ConsumerInfo →
// UpdateConsumer) so the cap self-heals across deploys without manual
// operator intervention. Consumers that don't exist yet are skipped:
// Subscribe creates them fresh with the correct cap.
func reconcilePreviewConsumers(js nats.JetStreamContext) error {
	for _, durable := range []string{"drive-preview", "drive-preview-priority", "drive-preview-standard"} {
		ci, err := js.ConsumerInfo(streamName, durable)
		if err != nil {
			if errors.Is(err, nats.ErrConsumerNotFound) {
				continue
			}
			return fmt.Errorf("consumer info %s: %w", durable, err)
		}
		if ci.Config.MaxDeliver == preview.QueueMaxDeliver {
			continue
		}
		cfg := ci.Config
		cfg.MaxDeliver = preview.QueueMaxDeliver
		if _, err := js.UpdateConsumer(streamName, &cfg); err != nil {
			return fmt.Errorf("update consumer %s: %w", durable, err)
		}
	}
	return nil
}

// subscribeAll wires a durable consumer for each subject. Durable
// names let the worker restart without losing checkpoint state.
// Each metrics.JobHandler is wrapped via InstrumentJob so the
// (subject, result) labels land on zkdrive_worker_jobs_total and
// the duration histogram captures wall time per subject.
func subscribeAll(ctx context.Context, wg *sync.WaitGroup, js nats.JetStreamContext, pool *pgxpool.Pool, m *metrics.Metrics, previewSvc *preview.Service, scanSvc *scan.Service, archiveSvc *retention.ArchiveService, indexSvc *index.Service, classifySvc *classify.Service, previewPriorityWorkers, previewStandardWorkers int) ([]*nats.Subscription, []func(), error) {
	// Webhook delivery worker. Constructed once and shared
	// across all webhook.events deliveries; the DeliveryClient
	// holds an http.Client + URLValidator that are both safe for
	// concurrent use. The repository is the pgx-backed
	// implementation which the worker runs WITHOUT setting
	// app.workspace_id so the RLS bypass branch fires (same
	// pattern as the audit archiver).
	webhookRepo := webhooks.NewPostgresRepository(pool)
	webhookDeliveryClient := webhooks.NewDeliveryClient(webhooks.NewURLValidator(), 0)
	webhookWorker, werr := webhooks.NewDeliveryWorker(webhookRepo, webhookDeliveryClient, m)
	if werr != nil {
		return nil, nil, fmt.Errorf("build webhook delivery worker: %w", werr)
	}

	subjects := []struct {
		subject string
		durable string
		handler nats.MsgHandler
		// extraOpts is appended to the base subscribe options
		// for this subject. Used to attach subject-specific
		// settings (e.g. MaxDeliver for the webhook consumer)
		// without leaking those settings onto unrelated drive
		// job consumers that have different retry semantics.
		extraOpts []nats.SubOpt
		// workers, when > 0, fans this subject's deliveries across
		// a fixed goroutine pool of that size so previews can be
		// rendered concurrently. 0 keeps the legacy single-goroutine
		// (inline callback) behaviour every other subject uses.
		workers int
	}{
		// QueueMaxDeliver caps how many times JetStream redelivers a
		// preview job that the handler keeps Nak'ing.
		// ErrUnsupportedMime is already Ack'd as a skip at attempt 1,
		// so this cap affects the "decode failed on bytes that will
		// never decode" path and — because previewHandler enforces
		// the per-tenant budget on every preview subject — the
		// budget DEFERRAL path (NakWithDelay). The legacy subject
		// uses QueueMaxDeliver (not the smaller preview.MaxDeliver)
		// for the same reason as the priority/standard subjects: the
		// API still publishes all previews here via PublishPreview →
		// drive.preview.generate, so capping at 5 would drop a
		// legitimately over-budget workspace's previews after a few
		// minutes of backoff instead of deferring them until its
		// window drains.
		//
		// SubjectPreview (the legacy single subject) is retained so
		// jobs published by an un-upgraded API server keep being
		// processed during a rolling deploy. New, tier-routed jobs
		// arrive on the priority / standard subjects below.
		{jobs.SubjectPreview, "drive-preview", m.InstrumentJob(ctx, jobs.SubjectPreview, traceJob(jobs.SubjectPreview, previewHandler(pool, previewSvc))), []nats.SubOpt{nats.MaxDeliver(preview.QueueMaxDeliver)}, 0},
		// Priority (Business / Secure-Business) and standard (Free /
		// Starter) preview subjects. Each gets its own durable
		// consumer and its own goroutine pool; the priority pool is
		// sized larger (default 6 vs 2) so paid tiers get ~3x the
		// render concurrency. QueueMaxDeliver is higher than the
		// legacy cap because Naks here include budget DEFERRALS, not
		// just decode failures (see preview.QueueMaxDeliver).
		{jobs.SubjectPreviewPriority, "drive-preview-priority", m.InstrumentJob(ctx, jobs.SubjectPreviewPriority, traceJob(jobs.SubjectPreviewPriority, previewHandler(pool, previewSvc))), []nats.SubOpt{nats.MaxDeliver(preview.QueueMaxDeliver)}, previewPriorityWorkers},
		{jobs.SubjectPreviewStandard, "drive-preview-standard", m.InstrumentJob(ctx, jobs.SubjectPreviewStandard, traceJob(jobs.SubjectPreviewStandard, previewHandler(pool, previewSvc))), []nats.SubOpt{nats.MaxDeliver(preview.QueueMaxDeliver)}, previewStandardWorkers},
		{jobs.SubjectScan, "drive-scan", m.InstrumentJob(ctx, jobs.SubjectScan, traceJob(jobs.SubjectScan, scanHandler(pool, scanSvc))), nil, 0},
		{jobs.SubjectIndex, "drive-index", m.InstrumentJob(ctx, jobs.SubjectIndex, traceJob(jobs.SubjectIndex, indexHandler(pool, indexSvc))), nil, 0},
		{jobs.SubjectArchive, "drive-archive", m.InstrumentJob(ctx, jobs.SubjectArchive, traceJob(jobs.SubjectArchive, archiveHandler(archiveSvc))), nil, 0},
		{jobs.SubjectClassify, "drive-classify", m.InstrumentJob(ctx, jobs.SubjectClassify, traceJob(jobs.SubjectClassify, classifyHandler(pool, classifySvc))), nil, 0},
		// MaxDeliver is server-side defense-in-depth for the
		// webhook consumer. The application-side counter in
		// internal/webhooks/worker.go also returns "dropped" once
		// attempt >= MaxAttempts, which the webhookDeliveryHandler
		// translates to msg.Term(). If that counter is ever bypassed
		// (programmer error, a deterministic remarshal failure that
		// historically returned "error" instead of "dropped", etc.),
		// MaxDeliver=MaxAttempts ensures JetStream itself caps
		// redeliveries at the same number rather than retrying for
		// up to MaxAge (7 days). Sized identically to the
		// application-side cap so the two layers agree.
		{webhooks.SubjectEvents, "webhook-deliveries", m.InstrumentJob(ctx, webhooks.SubjectEvents, traceJob(webhooks.SubjectEvents, webhookDeliveryHandler(webhookWorker))), []nats.SubOpt{nats.MaxDeliver(webhooks.MaxAttempts)}, 0},
	}
	var subs []*nats.Subscription
	// drains holds one pool-drain hook per pooled subject. run()
	// invokes them after unsubscribeAll but before nc.Drain() so
	// in-flight handlers ack while the connection is still open.
	var drains []func()
	for _, s := range subjects {
		opts := []nats.SubOpt{
			nats.Durable(s.durable),
			nats.AckWait(ackWait),
			nats.DeliverAll(),
			nats.ManualAck(),
		}
		opts = append(opts, s.extraOpts...)
		handler := s.handler
		if s.workers > 0 {
			var drain func()
			handler, drain = startJobPool(ctx, wg, s.workers, handler)
			drains = append(drains, drain)
		}
		sub, err := js.Subscribe(s.subject, handler, opts...)
		if err != nil {
			// Symmetric cleanup: tear down what this call created
			// before returning. unsubscribeAll stops delivery on the
			// already-bound subjects, then the pool drains (including
			// this subject's, appended just above) stop their
			// goroutines and wait out any handler that slipped in
			// during the brief window. Same unsubscribe→drain order
			// run() uses on shutdown; without it the earlier pools
			// would linger until run()'s deferred cancel reaps them.
			unsubscribeAll(subs)
			for _, drain := range drains {
				drain()
			}
			return nil, nil, fmt.Errorf("subscribe %s: %w", s.subject, err)
		}
		subs = append(subs, sub)
	}
	return subs, drains, nil
}

// startJobPool fans a single subject's deliveries across a fixed pool
// of `workers` goroutines so a busy subject (e.g. the priority preview
// queue) processes up to `workers` jobs concurrently rather than one
// at a time on the NATS delivery goroutine. It returns two values:
//
//   - handler: the subscription callback. It hands each message to the
//     pool over an unbuffered channel, which blocks (back-pressure)
//     while all workers are busy so JetStream's in-flight ceiling is
//     respected.
//   - drain: a shutdown hook that stops the pool from accepting new
//     messages and BLOCKS until every worker has finished the h(msg)
//     call it is currently running. Calling drain before nc.Drain()
//     guarantees in-flight handlers complete their msg.Ack/Nak/Term
//     while the NATS connection is still open, so a job that was mid
//     render at shutdown is acked rather than silently redelivered.
//
// Pool goroutines are also tracked on wg and select on ctx.Done(), so
// even if drain is never called the worker's bgGoroutines.Wait() /
// cancel() shutdown barrier still reaps them — drain is the graceful
// path, ctx cancellation the backstop.
//
// drain closes a private `stop` channel (never the message channel) so
// a NATS callback blocked on the unbuffered send unblocks via stop
// instead of panicking on a send to a closed channel. drain is
// idempotent.
//
// Concurrent processing is safe because each job calls msg.Ack /
// Nak / Term itself (ManualAck) and the preview service holds no
// per-call mutable state; the AckWait window plus QueueMaxDeliver
// bound any redelivery of a message that was in flight when the
// worker shut down.
func startJobPool(ctx context.Context, wg *sync.WaitGroup, workers int, h nats.MsgHandler) (nats.MsgHandler, func()) {
	if workers < 1 {
		workers = 1
	}
	ch := make(chan *nats.Msg)
	stop := make(chan struct{})
	var poolWG sync.WaitGroup
	poolWG.Add(workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer poolWG.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-stop:
					return
				case msg := <-ch:
					h(msg)
				}
			}
		}()
	}
	handler := func(msg *nats.Msg) {
		select {
		case <-ctx.Done():
		case <-stop:
		case ch <- msg:
		}
	}
	var stopOnce sync.Once
	drain := func() {
		stopOnce.Do(func() { close(stop) })
		poolWG.Wait()
	}
	return handler, drain
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
func previewHandler(pool *pgxpool.Pool, svc *preview.Service) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
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
			if errors.Is(err, preview.ErrBudgetExceeded) {
				// The workspace is over its per-window preview
				// budget. Re-enqueue with an exponential backoff
				// (capped at 5m) so the job is rendered later once
				// the window drains, rather than dropped or hot-
				// looped. The delay grows with the delivery count,
				// which QueueMaxDeliver bounds so a permanently
				// over-budget tenant's oldest jobs still eventually
				// terminate rather than accumulating forever.
				delay := preview.BudgetBackoff(deliveryAttempt(msg))
				slog.Info("worker preview budget exceeded, deferring",
					"file_id", job.FileID, "version_id", job.VersionID, "delay", delay.String())
				_ = msg.NakWithDelay(delay)
				return metrics.JobResultError
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

// deliveryAttempt returns the 1-based JetStream delivery count for
// msg, used to ramp the budget-deferral backoff. A push-consumer
// message always carries JetStream metadata; if it is somehow absent
// (e.g. a non-JetStream message in a test) we fall back to attempt 1
// so the first (shortest) backoff is applied rather than panicking.
func deliveryAttempt(msg *nats.Msg) int {
	md, err := msg.Metadata()
	if err != nil || md == nil {
		return 1
	}
	return int(md.NumDelivered)
}

// scanHandler decodes the FileJob envelope and runs the scan service.
// Successful verdicts (clean / quarantined) are acked; transient
// failures (pending — typically clamd connectivity errors) are Nak'd
// so NATS redelivers on the next AckWait cycle. The final status is
// persisted to file_versions so operators can audit results via SQL.
func scanHandler(pool *pgxpool.Pool, svc *scan.Service) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
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
func archiveHandler(svc *retention.ArchiveService) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
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

// gcInterval reads GC_INTERVAL_MINUTES and returns the cadence for
// the in-process orphan-object GC loop. Returns 0 only when the env
// var parses to a zero integer (documented opt-out for deploys
// where a dedicated K8s CronJob owns the GC). Default is 360 minutes
// (6h): short enough that an orphan from a quota-rejected confirm
// doesn't accumulate beyond a single trading day, long enough that
// the periodic DeleteObject round-trips are well below the noise
// floor of regular S3 traffic. Falling back to the default on
// parse failure is the conservative choice for the same reason as
// reconcileInterval.
func gcInterval() time.Duration {
	raw := os.Getenv("GC_INTERVAL_MINUTES")
	if raw == "" {
		return 360 * time.Minute
	}
	mins, err := strconv.Atoi(raw)
	if err != nil || mins < 0 {
		slog.Warn("worker invalid GC_INTERVAL_MINUTES; defaulting to 360", "raw", raw)
		return 360 * time.Minute
	}
	return time.Duration(mins) * time.Minute
}

// gcPendingUploadTTL reads GC_PENDING_UPLOAD_TTL_HOURS and returns
// the cooldown before a pending upload row is considered an orphan.
// Default is 168 hours (7 days) to match the trash / recycle-bin
// window used elsewhere. Operators tightening this below the
// presigned URL expiry (15 minutes) risks racing a still-uploading
// client; the package's DefaultPendingUploadTTL doc covers the
// trade-off. Falling back to the default on parse failure is the
// conservative choice (a typo'd value silently reclaiming live
// uploads is the worse outcome).
func gcPendingUploadTTL() time.Duration {
	raw := os.Getenv("GC_PENDING_UPLOAD_TTL_HOURS")
	if raw == "" {
		return gc.DefaultPendingUploadTTL
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours <= 0 {
		slog.Warn("worker invalid GC_PENDING_UPLOAD_TTL_HOURS; defaulting to 168", "raw", raw)
		return gc.DefaultPendingUploadTTL
	}
	return time.Duration(hours) * time.Hour
}

// workerStorageResolver adapts a *storage.ClientFactory to the
// narrow gc.StorageResolver interface. ForWorkspace returns the
// per-tenant client when a workspace_storage_credentials row
// exists, otherwise the shared fallback (S3_* env vars). On error
// — e.g. an unknown DB error during the lookup — the resolver
// returns nil so the GC skips object deletion for this workspace
// but still reclaims the metadata row. The structured log line
// below carries the underlying error for operator triage.
func workerStorageResolver(factory *storage.ClientFactory) gc.StorageResolver {
	return func(ctx context.Context, workspaceID uuid.UUID) gc.StorageDeleter {
		client, err := factory.ForWorkspace(ctx, workspaceID)
		if err != nil {
			// ErrNoCredentials is the legitimate "workspace
			// has no per-tenant client AND no fallback was
			// configured" path. It's not noisy enough to spam
			// for every GC run, so only the unexpected branch
			// gets logged.
			if errors.Is(err, storage.ErrNoCredentials) {
				return nil
			}
			slog.Warn("worker GC storage lookup failed", "workspace_id", workspaceID, "err", err)
			return nil
		}
		return client
	}
}

// runOrphanGC drives GCService.GCAll on a fixed cadence. The first
// run is delayed 90s after startup so the worker has time to
// connect NATS + JetStream subscribers (which carry the upload
// confirmations the GC must NOT race against on a freshly-restarted
// instance). The 90-second delay is intentionally longer than the
// reconciler's 60-second delay because the GC's predicate-guarded
// DELETE is sensitive to confirmation timing and a slower warmup is
// the cheaper failure mode.
//
// Cadence note matches runStorageReconciler: time.NewTicker means
// the loop body counts towards the next tick. If GCAll ever exceeds
// `interval` (would require ~thousands of orphans × per-DeleteObject
// latency), runs become back-to-back rather than at-most-once-per-
// interval. Acceptable because GCAll is idempotent (predicate guard
// + DeleteObject idempotency) and the loop body is synchronous.
func runOrphanGC(ctx context.Context, svc *gc.GCService, m *metrics.Metrics, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(90 * time.Second):
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		start := time.Now()
		summary, err := svc.GCAll(ctx)
		m.RecordGCRun(summary, err, start)
		for _, e := range summary.Errors {
			slog.Error("worker GC per-workspace failure", "workspace_id", e.WorkspaceID, "err", e.Err)
		}
		if err == nil || !errors.Is(err, context.Canceled) {
			slog.Info("worker orphan GC ran",
				"workspaces", summary.Workspaces,
				"orphans_found", summary.OrphansFound,
				"orphans_deleted", summary.OrphansDeleted,
				"objects_deleted", summary.ObjectsDeleted,
				"errors", len(summary.Errors),
				"duration", time.Since(start).Round(time.Millisecond).String(),
			)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("worker orphan GC aborted", "err", err)
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
// concurrent run. The reconciler_runtime_seconds metric is the
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
func indexHandler(pool *pgxpool.Pool, svc *index.Service) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
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
func classifyHandler(pool *pgxpool.Pool, svc *classify.Service) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
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

// traceJob adapts a metrics.JobHandler (which now takes a per-message
// ctx) into the same signature, but with a leading tracing wrapper
// that extracts the W3C trace-context from msg.Header and starts a
// consumer-kind span around the handler. The span name and result
// attribute flow through tracing.WrapConsumer's handler signature,
// which uses string results — we translate from / to
// metrics.JobResult here so cmd/worker/main.go stays the only
// site that imports both packages.
//
// When tracing is disabled (no-op provider), the WrapConsumer
// wrapper still runs — it just creates no-op spans whose
// SetAttributes / SetStatus calls are silent. The only cost is one
// propagator Extract per message, which is microsecond-level.
func traceJob(subject string, h metrics.JobHandler) metrics.JobHandler {
	wrapped := tracing.WrapConsumer(subject, func(ctx context.Context, msg *nats.Msg) string {
		return string(h(ctx, msg))
	})
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
		return metrics.JobResult(wrapped(ctx, msg))
	}
}

// webhookDeliveryHandler adapts webhooks.DeliveryWorker.Consume to
// the JobHandler signature. Per the JobHandler contract in
// internal/metrics/worker.go, the handler is responsible for calling
// msg.Ack / Nak / Term itself — we translate Consume's string result:
//
//   - "ok" / "skip": Ack — fully fanned out OR no matching
//     subscribers (skip).
//   - "error":       NakWithDelay — at least one subscriber failed
//     and the attempt count hasn't hit MaxAttempts yet. We pass the
//     same BackoffDelay schedule the worker recorded on the
//     next_retry_at column so JetStream's redelivery matches what
//     the admin UI tells the operator (1s, 2s, 4s, 8s between
//     retries 2 through 5). Without the delay JetStream redelivers
//     instantly and the documented schedule becomes a lie.
//   - "dropped":     Term — terminal (poison payload or MaxAttempts
//     exhausted); stop redelivery so the stream doesn't spin on an
//     undelivable event. Operators see the final state on the
//     webhook_deliveries row.
func webhookDeliveryHandler(w *webhooks.DeliveryWorker) metrics.JobHandler {
	return func(ctx context.Context, msg *nats.Msg) metrics.JobResult {
		result := w.Consume(ctx, msg)
		switch result {
		case "ok", "skip":
			_ = msg.Ack()
		case "dropped":
			_ = msg.Term()
		default:
			// Compute the same backoff the worker recorded on
			// next_retry_at. NumDelivered is 1-indexed (1 ==
			// initial attempt) so BackoffDelay(attempt+1)
			// computes the delay before the NEXT redelivery.
			attempt := 1
			if md, mdErr := msg.Metadata(); mdErr == nil && md != nil {
				attempt = int(md.NumDelivered)
				if attempt < 1 {
					attempt = 1
				}
			}
			_ = msg.NakWithDelay(webhooks.BackoffDelay(attempt + 1))
		}
		return metrics.JobResult(result)
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
