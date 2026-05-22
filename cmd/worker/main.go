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
	"log"
	"os"
	"os/signal"
	"strconv"
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
		log.Fatalf("worker exited: %v", err)
	}
}

func run() error {
	log.Printf("zk-drive worker version=%s", version.Version)

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
		log.Printf("worker: storage client wired to %s (bucket=%s)", cfg.S3Endpoint, cfg.S3Bucket)
	} else {
		log.Printf("worker: S3_ENDPOINT unset; preview/scan jobs will be logged only")
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
	if interval := reconcileInterval(); interval > 0 {
		bgGoroutines.Add(1)
		rc := reconciler.New(pool)
		go func() {
			defer bgGoroutines.Done()
			runStorageReconciler(ctx, rc, interval)
		}()
	}

	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = defaultNATS
	}

	nc, err := nats.Connect(natsURL,
		nats.Name("zk-drive-worker"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		// Bound shutdown drain time so a single slow / hung
		// message handler (e.g. a multi-minute ClamAV scan that
		// just started when SIGTERM arrived) can't block the
		// entire shutdown chain. The LIFO defer sequence below
		// runs nc.Drain() before cancel() so well-behaved
		// handlers get to finish gracefully, but if the drain
		// hangs past 20s NATS forcibly closes the connection,
		// our defers continue, and the pod still terminates
		// well within the K8s default terminationGracePeriod of
		// 30s rather than being SIGKILL'd mid-shutdown.
		nats.DrainTimeout(20*time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect nats %s: %w", natsURL, err)
	}
	defer nc.Drain() //nolint:errcheck // best-effort drain bounded by nats.DrainTimeout

	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	if err := ensureStream(js); err != nil {
		return fmt.Errorf("ensure stream: %w", err)
	}

	subs, err := subscribeAll(ctx, js, pool, previewSvc, scanSvc, archiveSvc, indexSvc, classifySvc)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer unsubscribeAll(subs)

	log.Printf("zk-drive worker listening on %s (stream=%s)", natsURL, streamName)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("received signal %s, shutting down", sig)
	return nil
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
func subscribeAll(ctx context.Context, js nats.JetStreamContext, pool *pgxpool.Pool, previewSvc *preview.Service, scanSvc *scan.Service, archiveSvc *retention.ArchiveService, indexSvc *index.Service, classifySvc *classify.Service) ([]*nats.Subscription, error) {
	subjects := []struct {
		subject string
		durable string
		handler nats.MsgHandler
	}{
		{jobs.SubjectPreview, "drive-preview", previewHandler(ctx, pool, previewSvc)},
		{jobs.SubjectScan, "drive-scan", scanHandler(ctx, pool, scanSvc)},
		{jobs.SubjectIndex, "drive-index", indexHandler(ctx, pool, indexSvc)},
		{jobs.SubjectArchive, "drive-archive", archiveHandler(ctx, archiveSvc)},
		{jobs.SubjectClassify, "drive-classify", classifyHandler(ctx, pool, classifySvc)},
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
// service. Unsupported mime types (ErrUnsupportedMime) ack without
// error because the file is simply not previewable; every other
// failure Nak's so NATS redelivers on the next AckWait cycle.
func previewHandler(ctx context.Context, pool *pgxpool.Pool, svc *preview.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed preview payload: %v", err)
			_ = msg.Term()
			return
		}
		if isStrictZK(ctx, pool, job.FileID) {
			log.Printf("worker: skipping strict-zk file (preview) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		if svc == nil {
			log.Printf("worker: preview skipped (no storage client): file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		p, err := svc.Generate(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			if errors.Is(err, preview.ErrUnsupportedMime) {
				log.Printf("worker: preview unsupported mime: file=%s version=%s", job.FileID, job.VersionID)
				_ = msg.Ack()
				return
			}
			log.Printf("worker: preview failed file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: preview ok file=%s version=%s key=%s", job.FileID, job.VersionID, p.ObjectKey)
		_ = msg.Ack()
	}
}

// scanHandler decodes the FileJob envelope and runs the scan service.
// Successful verdicts (clean / quarantined) are acked; transient
// failures (pending — typically clamd connectivity errors) are Nak'd
// so NATS redelivers on the next AckWait cycle. The final status is
// persisted to file_versions so operators can audit results via SQL.
func scanHandler(ctx context.Context, pool *pgxpool.Pool, svc *scan.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed scan payload: %v", err)
			_ = msg.Term()
			return
		}
		if isStrictZK(ctx, pool, job.FileID) {
			log.Printf("worker: skipping strict-zk file (scan) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		if svc == nil {
			log.Printf("worker: scan skipped (no storage client): file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		v, err := svc.Scan(jobCtx, job.FileID, job.VersionID)
		if err != nil {
			log.Printf("worker: scan error file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: scan %s file=%s version=%s detail=%q", v.Status, job.FileID, job.VersionID, v.Detail)
		_ = msg.Ack()
	}
}

// archiveHandler compresses and uploads a single version's bytes to
// the cold archive key pattern, then stamps archived_at on the row.
// Missing storage client -> ack and move on (the same pattern as
// preview/scan).
func archiveHandler(ctx context.Context, svc *retention.ArchiveService) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed archive payload: %v", err)
			_ = msg.Term()
			return
		}
		if svc == nil {
			log.Printf("worker: archive skipped (no storage client): file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := svc.ArchiveVersion(jobCtx, job.VersionID); err != nil {
			log.Printf("worker: archive failed file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: archive ok file=%s version=%s", job.FileID, job.VersionID)
		_ = msg.Ack()
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
			log.Printf("worker: guest expiry sweep failed: %v", err)
		} else if revoked > 0 {
			log.Printf("worker: guest expiry sweep revoked %d permissions", revoked)
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
		log.Printf("worker: invalid RECONCILE_INTERVAL_MINUTES=%q; defaulting to 60", raw)
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
func runStorageReconciler(ctx context.Context, rc *reconciler.Reconciler, interval time.Duration) {
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
			log.Printf("worker: reconciler workspace=%s err=%v", e.WorkspaceID, e.Err)
		}
		if err == nil || !errors.Is(err, context.Canceled) {
			log.Printf("worker: reconciler ran workspaces=%d updated=%d drift_bytes=%d errors=%d duration=%s",
				summary.Workspaces, summary.Updated, summary.TotalDriftBytes, len(summary.Errors),
				time.Since(start).Round(time.Millisecond))
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("worker: storage reconciler aborted: %v", err)
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
func indexHandler(ctx context.Context, pool *pgxpool.Pool, svc *index.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed index payload: %v", err)
			_ = msg.Term()
			return
		}
		if isStrictZK(ctx, pool, job.FileID) {
			log.Printf("worker: skipping strict-zk file (index) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		if svc == nil {
			log.Printf("worker: index acked (no storage) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := svc.IndexFile(jobCtx, job.FileID, job.VersionID); err != nil {
			log.Printf("worker: index failed file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: index ok file=%s version=%s", job.FileID, job.VersionID)
		_ = msg.Ack()
	}
}

// classifyHandler decodes the FileJob envelope and runs the
// classification service. Strict-ZK files skip + ack so the server
// never writes a label derived from plaintext it does not hold.
func classifyHandler(ctx context.Context, pool *pgxpool.Pool, svc *classify.Service) nats.MsgHandler {
	return func(msg *nats.Msg) {
		var job jobs.FileJob
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			log.Printf("worker: malformed classify payload: %v", err)
			_ = msg.Term()
			return
		}
		if isStrictZK(ctx, pool, job.FileID) {
			log.Printf("worker: skipping strict-zk file (classify) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		if svc == nil {
			log.Printf("worker: classify acked (no pool) file=%s version=%s", job.FileID, job.VersionID)
			_ = msg.Ack()
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		if err := svc.Classify(jobCtx, job.FileID); err != nil {
			log.Printf("worker: classify failed file=%s version=%s: %v", job.FileID, job.VersionID, err)
			_ = msg.Nak()
			return
		}
		log.Printf("worker: classify ok file=%s version=%s", job.FileID, job.VersionID)
		_ = msg.Ack()
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
		log.Printf("worker: lookup encryption mode for file=%s: %v", fileID, err)
		return false
	}
	return mode == folder.EncryptionStrictZK
}
