// zk-drive orphan-object GC binary.
//
// Standalone entrypoint that reclaims storage for presigned PUTs
// that completed but were never confirmed (see internal/gc for the
// reclaim contract). Designed to be run as a Kubernetes CronJob (or
// Compose service) on a regular cadence — every workspace's orphan
// rows are scanned and reclaimed in O(N) DeleteObject + DELETE row
// operations, capped by the per-workspace orphan limit so a single
// run cannot monopolise the DB pool indefinitely.
//
// Operational characteristics mirror cmd/reconciler:
//
//   - Idempotent: re-running against a workspace with no orphans
//     is a no-op (single index-only SELECT, nothing else).
//   - Bounded blast radius: predicate-guarded DELETE means a row
//     that was confirmed between the scan and the delete is not
//     removed; DeleteObject is idempotent in S3 so retrying a
//     failed delete is safe.
//   - Best-effort across workspaces: a single sick workspace
//     (e.g. unreachable per-tenant zk-object-fabric) is logged and
//     skipped; the rest of the population is still reclaimed.
//   - Per-workspace failures do NOT flip the exit code: same
//     rationale as the reconciler — the metrics-based alerting
//     path (gc_workspace_errors_total) is the operator signal.
//     cmd/orphan-gc intentionally does NOT export /metrics
//     because the process exits as soon as the run completes;
//     no scrape interval would catch it. K8s Job status is the
//     alerting signal for this binary.
//   - Exits non-zero in three cases: (a) configuration / pool
//     connect failure, (b) the workspaces enumeration query
//     itself failed, and (c) the run was interrupted by SIGTERM
//     / context cancellation before every workspace had been
//     visited.
//
// Configuration: reads DATABASE_URL, S3_*, and CREDENTIAL_CODEC_*
// from the environment via internal/config.Load + crypto.LoadFromEnv.
// GC_PENDING_UPLOAD_TTL_HOURS overrides the orphan cooldown; default
// is 168 hours (7 days). GC_INTERVAL_MINUTES is NOT read here — the
// CronJob schedule controls cadence externally.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/config"
	cryptopkg "github.com/kennguy3n/zk-drive/internal/crypto"
	"github.com/kennguy3n/zk-drive/internal/database"
	driveFile "github.com/kennguy3n/zk-drive/internal/file"
	"github.com/kennguy3n/zk-drive/internal/gc"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("orphan-gc exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("orphan-gc")
	slog.Info("zk-drive orphan-gc starting", "version", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

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
	} else {
		slog.Warn("orphan-gc S3_ENDPOINT unset; metadata-only reclaim, S3 objects will remain")
	}

	credentialCodec, err := cryptopkg.LoadFromEnv()
	if err != nil {
		return fmt.Errorf("credential codec: %w", err)
	}
	storageFactory := storage.NewClientFactory(pool, storageClient, credentialCodec)

	fileSvc := driveFile.NewService(driveFile.NewPostgresRepository(pool))

	gcSvc := gc.New(pool, fileSvc, resolver(storageFactory), gc.WithTTL(gcPendingUploadTTL()))

	start := time.Now()
	summary, runErr := gcSvc.GCAll(ctx)

	// Surface per-workspace errors and the run summary BEFORE
	// deciding on the function-level err — same rationale as
	// cmd/reconciler. Partial data is useful for the next
	// operator triage pass.
	for _, e := range summary.Errors {
		slog.Error("orphan-gc per-workspace failure", "workspace_id", e.WorkspaceID, "err", e.Err)
	}
	slog.Info("orphan-gc completed",
		"workspaces", summary.Workspaces,
		"orphans_found", summary.OrphansFound,
		"orphans_deleted", summary.OrphansDeleted,
		"objects_deleted", summary.ObjectsDeleted,
		"errors", len(summary.Errors),
		"duration", time.Since(start).Round(time.Millisecond).String(),
	)

	if runErr != nil {
		return fmt.Errorf("gc all: %w", runErr)
	}
	return nil
}

// gcPendingUploadTTL mirrors the worker's helper so the standalone
// binary respects the same env var (cleanly comparable in
// kubectl logs grep for "ttl" across both deployments).
func gcPendingUploadTTL() time.Duration {
	raw := os.Getenv("GC_PENDING_UPLOAD_TTL_HOURS")
	if raw == "" {
		return gc.DefaultPendingUploadTTL
	}
	hours, err := strconv.Atoi(raw)
	if err != nil || hours <= 0 {
		slog.Warn("orphan-gc invalid GC_PENDING_UPLOAD_TTL_HOURS; defaulting to 168", "raw", raw)
		return gc.DefaultPendingUploadTTL
	}
	return time.Duration(hours) * time.Hour
}

// resolver adapts storage.ClientFactory to gc.StorageResolver. Same
// shape as cmd/worker's workerStorageResolver — duplicated rather
// than shared because the standalone binary's import surface is
// intentionally minimal (no NATS, no preview/scan/index/archive
// services), and a 15-line adapter doesn't justify a separate
// package. The two implementations MUST stay in sync on the
// ErrNoCredentials triage: it is the only "expected nil" path
// (workspace has no per-tenant client AND no shared fallback) and
// must not log; every other error is a real operator signal and
// MUST be logged — without this branch the standalone binary
// silently swallows decrypt failures, DB connectivity errors, and
// misconfigured credential codecs with the only visible symptom
// being ObjectsDeleted < OrphansDeleted in the run summary log line
// (cmd/orphan-gc does not export /metrics, so the metric-divergence
// signal the worker relies on is not available here).
func resolver(factory *storage.ClientFactory) gc.StorageResolver {
	return func(ctx context.Context, workspaceID uuid.UUID) gc.StorageDeleter {
		client, err := factory.ForWorkspace(ctx, workspaceID)
		if err != nil {
			if errors.Is(err, storage.ErrNoCredentials) {
				return nil
			}
			slog.Warn("orphan-gc storage lookup failed", "workspace_id", workspaceID, "err", err)
			return nil
		}
		return client
	}
}
