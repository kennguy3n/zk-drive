// zk-drive audit-log archiver binary — Phase 5 / WS-23.
//
// Standalone entrypoint that archives audit_log rows older than the
// configured retention window to S3 as compressed JSONL (one object
// per workspace per calendar month) and deletes them from the hot
// table. Designed to be run as a Kubernetes CronJob (or Compose
// service) on a nightly cadence — every workspace's expired rows
// are batched in O(rows / MaxRowsPerBatch) PUT + DELETE pairs.
//
// Operational characteristics mirror cmd/reconciler + cmd/orphan-gc:
//
//   - Idempotent: a crash between S3 upload and audit_log DELETE
//     leaves the rows in place; the next run re-uploads them to a
//     fresh UUID-suffixed object (no row lost; the cold tier may
//     carry duplicates which the restore tool dedupes by id).
//   - Bounded blast radius: the archiver only writes to S3 and
//     deletes from audit_log; it does not mutate any user-facing
//     table. Per-workspace timeout (default 30 min) prevents one
//     wedged tenant from starving the rest of the population.
//   - Best-effort across workspaces: a single sick workspace
//     (e.g. unreachable S3 endpoint, schema corruption) is logged
//     as an error and skipped; the rest of the population is still
//     archived.
//   - Per-bucket failures DO flip the exit code (non-zero) once
//     any bucket fails — same K8s Job alerting signal as the
//     reconciler. Successful runs return zero even when the
//     enumerate query returned zero buckets (a healthy "nothing
//     to archive yet" state on a fresh install).
//   - Opt-in: AUDIT_LOG_ARCHIVE_ENABLED must be set to a truthy
//     value. The binary refuses to run otherwise so a misconfigured
//     CronJob can't start deleting audit history before the
//     operator has confirmed retention + S3 prefix.
//
// Configuration: reads DATABASE_URL, S3_*, AUDIT_LOG_* from the
// environment via internal/config.Load. Optional: OTEL_* for
// tracing (each archiver run + each (workspace, month) bucket is a
// span).
//
// IAM requirements: the configured S3 credentials need s3:PutObject
// on the archive bucket + prefix. No ListBucket or GetObject is
// needed for the archive write path (those are exercised by
// cmd/audit-restore).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kennguy3n/zk-drive/internal/audit"
	"github.com/kennguy3n/zk-drive/internal/config"
	"github.com/kennguy3n/zk-drive/internal/database"
	"github.com/kennguy3n/zk-drive/internal/logging"
	"github.com/kennguy3n/zk-drive/internal/metrics"
	"github.com/kennguy3n/zk-drive/internal/storage"
	"github.com/kennguy3n/zk-drive/internal/tracing"
	"github.com/kennguy3n/zk-drive/internal/version"
)

func main() {
	if err := run(); err != nil {
		slog.Error("audit-archiver exited", "err", err)
		os.Exit(1)
	}
}

func run() error {
	logging.Init("audit-archiver")
	slog.Info("zk-drive audit-archiver starting", "version", version.Version)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if !cfg.AuditArchiveEnabled {
		// Opt-in safety floor: the operator must explicitly
		// enable archival via AUDIT_LOG_ARCHIVE_ENABLED. A
		// CronJob that runs this binary with the env var unset
		// will exit zero (success) so the K8s Job is not flagged
		// as Failed — the run is a no-op, not a misconfiguration
		// the operator needs to be paged about. The startup log
		// makes the no-op visible to anyone reading kubectl logs.
		slog.Info("audit-archiver disabled by AUDIT_LOG_ARCHIVE_ENABLED; exiting zero")
		return nil
	}

	if strings.TrimSpace(cfg.S3Endpoint) == "" {
		return errors.New("audit-archiver requires S3_ENDPOINT to be configured")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Tracing init BEFORE any other subsystem — same pattern as
	// cmd/server and cmd/worker. When OTEL_EXPORTER_OTLP_ENDPOINT
	// is unset, tracing.Init returns a no-op provider so the
	// archiver runs fine without a collector wired up.
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
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	// Build the storage client. The archive bucket overrides
	// S3_BUCKET when configured so an operator can target a
	// dedicated bucket with Glacier transition / object-lock
	// retention rules without affecting live file storage.
	archiveBucket := cfg.S3Bucket
	if cfg.AuditArchiveBucket != "" {
		archiveBucket = cfg.AuditArchiveBucket
	}
	storageClient, err := storage.NewClient(storage.Config{
		Endpoint:  cfg.S3Endpoint,
		Bucket:    archiveBucket,
		AccessKey: cfg.S3AccessKey,
		SecretKey: cfg.S3SecretKey,
	})
	if err != nil {
		return fmt.Errorf("storage client: %w", err)
	}

	m := metrics.New()
	repo := audit.NewPostgresArchiveRepository(pool)
	svc, err := audit.NewArchiveService(repo, storageClient, audit.ArchiveServiceConfig{
		RetentionDays:   cfg.AuditLogRetentionDays,
		ArchivePrefix:   cfg.AuditArchivePrefix,
		MaxRowsPerBatch: cfg.AuditArchiveMaxRowsPerBatch,
	})
	if err != nil {
		return fmt.Errorf("audit archive service: %w", err)
	}
	svc.WithMetrics(m)

	slog.Info("audit-archiver configured",
		"retention_days", cfg.AuditLogRetentionDays,
		"archive_bucket", archiveBucket,
		"archive_prefix", cfg.AuditArchivePrefix,
		"max_rows_per_batch", cfg.AuditArchiveMaxRowsPerBatch,
	)

	start := time.Now()
	result, runErr := svc.Run(ctx)
	duration := time.Since(start)

	// Categorise the run outcome for the metric so the operator
	// dashboard can plot "runs that completed but had partial
	// failures" separately from "runs that aborted entirely".
	metricResult := metrics.AuditArchiveResultOK
	switch {
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		metricResult = metrics.AuditArchiveResultCancelled
	case runErr != nil:
		metricResult = metrics.AuditArchiveResultError
	case result != nil && len(result.Errors) > 0:
		metricResult = metrics.AuditArchiveResultPartial
	}
	m.RecordAuditArchiveRun(metricResult, duration.Seconds())

	if result != nil {
		for _, e := range result.Errors {
			slog.Error("audit-archive bucket failure", "err", e)
		}
		slog.Info("audit-archiver completed",
			"run_id", result.RunID,
			"workspace_months_total", result.WorkspaceMonthsTotal,
			"workspace_months_ok", result.WorkspaceMonthsOK,
			"workspace_months_failed", result.WorkspaceMonthsFailed,
			"rows_archived", result.RowsArchived,
			"bytes_uploaded", result.BytesUploaded,
			"errors", len(result.Errors),
			"duration", duration.Round(time.Millisecond).String(),
		)
	}

	if runErr != nil {
		return fmt.Errorf("archive run: %w", runErr)
	}
	if result != nil && len(result.Errors) > 0 {
		return fmt.Errorf("%d bucket(s) failed", len(result.Errors))
	}
	return nil
}
