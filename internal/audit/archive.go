// Audit-log cold archival (WS-23).
//
// # Why this exists
//
// audit_log records every security-relevant event the system emits
// (login, permission grant/revoke, admin user management, retention-
// policy edit, MFA lifecycle, guest-invite email delivery). For SOC2
// Type II, HIPAA, and GDPR, operators need both a documented retention
// policy on the hot tier AND the ability to produce historical
// records on regulator request. Today the table is INSERT-only and
// unbounded — every event since boot is still on the primary, and
// "show me workspace X's admin actions between Jan 1 and Mar 31" is
// answered by scanning the whole table.
//
// # Design
//
// The archiver runs as a periodic K8s CronJob (cmd/audit-archiver/).
// For each invocation it:
//
//  1. Computes a single cutoff = now() - retention_days.
//  2. Enumerates (workspace, year-month) buckets with rows older
//     than cutoff via a GROUP BY query.
//  3. For each bucket: streams rows out in id-ordered pages and for
//     each page writes a JSONL.gz buffer in memory, uploads to S3 as
//     {prefix}{workspace_id}/{year-month}/{batch_id}.jsonl.gz (where
//     batch_id is a fresh per-page UUID, NOT the run-level runID),
//     records an audit_log_archive_runs row carrying that batch_id
//     as archive_object_key, then deletes the just-archived rows
//     from audit_log. Each page is its own independently-durable
//     commit unit; the per-batch UUID guarantees no two pages within
//     the same run can share an S3 key.
//
//     The run-level runID is preserved on every archive_runs record
//     so an operator can correlate "all batches from one CronJob
//     tick" via WHERE run_id = ?. The S3 key itself is keyed by
//     batch_id rather than runID because a single (workspace, month)
//     bucket may exceed MaxRowsPerBatch and split into multiple
//     pages — if those pages all shared runID in the key, page 2
//     would silently overwrite page 1 in S3 while page 1's rows had
//     already been deleted from the hot tier (permanent audit
//     data loss). See WS-23 PR #68 Devin Review finding
//     BUG_pr-review-job-92fe43f0a26c44ea817db9bacbc6c88d_0001 for
//     the original walkthrough.
//
// The (workspace, month) shard size is the load-bearing trade-off:
// large enough that the cold tier doesn't degrade into millions of
// tiny objects, small enough that one bucket fits comfortably in
// memory + one S3 PUT round-trip stays well under the exporter's
// context budget. A monthly shard at ~50k entries averages 5 MB
// compressed.
//
// # Idempotency
//
// The bucket loop commits in this order: PutObject → RecordRun →
// DeleteBatch. This order is chosen so the failure modes leak in
// the safest direction — preserving audit rows over preserving
// cold-tier cleanliness:
//
//   - Crash after PutObject, before RecordRun → S3 object is
//     orphaned but rows are still in the hot tier. The next run
//     re-archives them to a DIFFERENT object key (fresh batch_id
//     UUID suffix). Operators can sweep orphans by diffing
//     ListObjects against audit_log_archive_runs.archive_object_key.
//   - Crash after RecordRun, before DeleteBatch → both the S3
//     object and the run record committed, but rows are still in
//     the hot tier. The next run re-archives them under a new
//     batch_id; the cold tier carries duplicate objects which the
//     restore CLI dedupes by row id at read time.
//   - DeleteBatch failure (after PutObject + RecordRun both
//     succeeded) → same as the crash-after-RecordRun case: the
//     bucket is reported as failed in the RunResult, rows remain
//     in the hot tier, and the next run re-archives them.
//
// No audit row is ever lost: rows leave the hot tier ONLY after
// the S3 object and the run record are both durably committed.
//
// # RLS
//
// The archiver runs against the pool without setting app.workspace_id,
// so the migration-024 tenant_isolation policies fall through to the
// bypass branch (app_current_workspace_id() IS NULL). Direct admin
// queries from the in-app admin console still see only their own
// workspace's archive history.
package audit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// MinRetentionDays is the smallest hot-tier retention the archiver
// will execute against. Values below this likely indicate operator
// error (e.g. mistyped "1" for "100") and would aggressively prune
// legitimately-recent audit history. The CLI surfaces a clear error
// rather than silently archiving.
//
// Exported so internal/config can pin its clampAuditRetentionDays
// ceiling/floor to the same constant and reject malformed env vars
// at startup rather than letting them bubble up to NewArchiveService.
const MinRetentionDays = 7

// minRetentionDays is the unexported alias kept for compactness in
// archive.go's own callers. Both names point at the same value;
// MinRetentionDays is the canonical export.
const minRetentionDays = MinRetentionDays

// defaultMaxRowsPerBatch caps a single JSONL.gz upload. At ~500
// bytes per audit row, 50k rows produces a ~25 MB uncompressed /
// ~5 MB compressed object — well below S3's 5 GB single-PUT limit
// and small enough to fit in memory on a 256 MiB CronJob pod.
const defaultMaxRowsPerBatch = 50000

// ArchiveStorage is the slice of the storage client the archive
// service depends on. Defined locally so unit tests can supply an
// in-memory fake without dragging the full *storage.Client surface.
type ArchiveStorage interface {
	PutObject(ctx context.Context, objectKey, contentType string, body []byte) error
}

// ArchiveMetricsRecorder is the bounded surface the archive service
// uses to emit per-bucket observability counters. Defined locally so
// the audit package does not import internal/metrics directly (which
// would create a cycle when cmd/server wires both). The constants
// for the result strings live in internal/metrics; callers (the
// cmd/audit-archiver binary) pass the binding through.
type ArchiveMetricsRecorder interface {
	RecordAuditArchiveBucket(result string, rows int, bytes int64)
}

// archiveBucketResultOK / Error / Partial mirror the constants in
// internal/metrics. Defined as untyped string literals here so the
// archive service can emit metrics through the interface without
// importing the metrics package. internal/metrics' exported
// AuditArchiveBucketResult* constants MUST stay in sync — both
// places carry the same literal so a single source-of-truth audit
// can find them via grep.
//
// "partial" distinguishes the case where a bucket failed mid-way
// through its pagination loop AFTER one or more pages had already
// been durably committed to S3 + audit_log_archive_runs + the hot
// tier had been deleted. Those committed rows are real archive
// activity and must be counted in the rows/bytes totals; the
// label simply signals to operators that the bucket as a whole
// did not finish (the remaining rows will be archived on the
// next run).
const (
	archiveBucketResultOK      = "ok"
	archiveBucketResultError   = "error"
	archiveBucketResultPartial = "partial"
)

// ArchiveServiceConfig bundles archive-service tunables sourced from
// internal/config so the service stays env-var-free.
type ArchiveServiceConfig struct {
	RetentionDays    int
	ArchivePrefix    string
	MaxRowsPerBatch  int
	UploadTimeout    time.Duration
	WorkspaceTimeout time.Duration
}

// DefaultUploadTimeout caps a single S3 PUT. 60s leaves ample
// headroom over typical zk-object-fabric upload latency for a 25 MB
// object while still preventing one wedged tenant from pinning the
// run.
const DefaultUploadTimeout = 60 * time.Second

// DefaultWorkspaceTimeout caps the total time spent archiving any
// single workspace's history (across all of its monthly buckets).
// Stops a single workspace with millions of historical rows from
// starving the rest of the population in one CronJob tick. The
// archiver moves on to the next workspace when this fires; the
// abandoned workspace's remaining months are picked up by the next
// scheduled run.
const DefaultWorkspaceTimeout = 30 * time.Minute

// RunResult summarises one full archiver invocation. Surfaced as
// metrics (rows_total, bytes_total) and used by the CronJob exit
// code (Errors > 0 flips the K8s Job status to Failed).
type RunResult struct {
	RunID                  uuid.UUID
	CutoffTime             time.Time
	WorkspaceMonthsTotal   int
	WorkspaceMonthsOK      int
	WorkspaceMonthsFailed  int
	RowsArchived           int
	BytesUploaded          int64
	Errors                 []error
}

// ArchiveService orchestrates one archiver pass.
type ArchiveService struct {
	repo    ArchiveRepository
	storage ArchiveStorage
	metrics ArchiveMetricsRecorder
	cfg     ArchiveServiceConfig

	// nowFn lets tests pin the cutoff calculation. Production
	// passes time.Now.
	nowFn func() time.Time
}

// WithMetrics attaches a metrics recorder so the archive service
// emits per-bucket observability counters. Returns the service for
// chaining-style wiring; the mutation is also visible via the
// pointer receiver so callers can discard the return value. nil
// recorder is allowed for tests / no-op runs.
func (a *ArchiveService) WithMetrics(m ArchiveMetricsRecorder) *ArchiveService {
	a.metrics = m
	return a
}

// NewArchiveService constructs an ArchiveService. Returns an error
// if the config violates the safety floor (retention < minRetentionDays)
// or other invariant the caller is expected to satisfy.
func NewArchiveService(repo ArchiveRepository, storage ArchiveStorage, cfg ArchiveServiceConfig) (*ArchiveService, error) {
	if repo == nil {
		return nil, errors.New("audit archive: repository is required")
	}
	if storage == nil {
		return nil, errors.New("audit archive: storage client is required")
	}
	if cfg.RetentionDays < minRetentionDays {
		return nil, fmt.Errorf("audit archive: retention must be >= %d days, got %d", minRetentionDays, cfg.RetentionDays)
	}
	if strings.TrimSpace(cfg.ArchivePrefix) == "" {
		return nil, errors.New("audit archive: archive prefix is required")
	}
	if cfg.MaxRowsPerBatch <= 0 {
		cfg.MaxRowsPerBatch = defaultMaxRowsPerBatch
	}
	if cfg.UploadTimeout <= 0 {
		cfg.UploadTimeout = DefaultUploadTimeout
	}
	if cfg.WorkspaceTimeout <= 0 {
		cfg.WorkspaceTimeout = DefaultWorkspaceTimeout
	}
	// Force prefix to end with exactly one slash so callers can
	// concatenate per-workspace suffixes without bookkeeping.
	// internal/config normalises this already, but defence-in-
	// depth here lets a test or future caller bypass config.
	if !strings.HasSuffix(cfg.ArchivePrefix, "/") {
		cfg.ArchivePrefix += "/"
	}
	return &ArchiveService{
		repo:    repo,
		storage: storage,
		cfg:     cfg,
		nowFn:   time.Now,
	}, nil
}

// Run executes one archiver pass. Returns the aggregate result even
// when individual buckets fail — per-bucket errors are captured in
// RunResult.Errors so the caller can surface them as metrics +
// non-zero exit code without aborting other buckets.
func (a *ArchiveService) Run(ctx context.Context) (*RunResult, error) {
	tracer := otel.Tracer("github.com/kennguy3n/zk-drive/internal/audit")
	ctx, span := tracer.Start(ctx, "audit.ArchiveService.Run")
	defer span.End()

	runID := uuid.New()
	cutoff := a.nowFn().Add(-time.Duration(a.cfg.RetentionDays) * 24 * time.Hour).UTC()
	result := &RunResult{
		RunID:      runID,
		CutoffTime: cutoff,
	}
	span.SetAttributes(
		attribute.String("audit.archive.run_id", runID.String()),
		attribute.String("audit.archive.cutoff_time", cutoff.Format(time.RFC3339)),
		attribute.Int("audit.archive.retention_days", a.cfg.RetentionDays),
	)

	buckets, err := a.repo.EnumerateWorkspaceMonths(ctx, cutoff)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "enumerate workspace months failed")
		return result, fmt.Errorf("enumerate workspace months: %w", err)
	}
	result.WorkspaceMonthsTotal = len(buckets)
	span.SetAttributes(attribute.Int("audit.archive.buckets_total", len(buckets)))

	// Group buckets by workspace so we can apply WorkspaceTimeout
	// uniformly across one workspace's months and short-circuit a
	// runaway tenant without affecting peers.
	//
	// Defense-in-depth grouping: ArchiveRepository.EnumerateWorkspaceMonths
	// documents an ORDER BY workspace_id, year_month contract (see the
	// interface docstring + PostgresArchiveRepository's SQL), so adjacent
	// entries SHOULD share a workspace_id and a sequential collapse would
	// work. We deliberately don't rely on that here — a future repository
	// implementation (or a stale snapshot of one) returning unsorted rows
	// would otherwise silently split one workspace into multiple groups,
	// each starting its own WorkspaceTimeout clock. The two-pass map+slice
	// grouping below is O(n) and produces a single group per workspace
	// regardless of input ordering.
	type wsBuckets struct {
		workspaceID uuid.UUID
		months      []WorkspaceAuditMonth
	}
	groups := make([]wsBuckets, 0)
	indexByWS := make(map[uuid.UUID]int, len(buckets))
	for _, b := range buckets {
		idx, seen := indexByWS[b.WorkspaceID]
		if !seen {
			groups = append(groups, wsBuckets{workspaceID: b.WorkspaceID})
			idx = len(groups) - 1
			indexByWS[b.WorkspaceID] = idx
		}
		groups[idx].months = append(groups[idx].months, b)
	}

	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			span.SetStatus(codes.Error, "cancelled during workspace iteration")
			return result, err
		}
		a.archiveWorkspace(ctx, runID, cutoff, g.workspaceID, g.months, result)
	}

	if len(result.Errors) > 0 {
		span.SetStatus(codes.Error, fmt.Sprintf("%d bucket(s) failed", len(result.Errors)))
	}
	return result, nil
}

// archiveWorkspace processes every month bucket for one workspace
// under a per-workspace context timeout. Bucket failures are
// recorded but do not abort the workspace's other months.
func (a *ArchiveService) archiveWorkspace(
	ctx context.Context,
	runID uuid.UUID,
	cutoff time.Time,
	workspaceID uuid.UUID,
	months []WorkspaceAuditMonth,
	result *RunResult,
) {
	wsCtx, cancel := context.WithTimeout(ctx, a.cfg.WorkspaceTimeout)
	defer cancel()
	tracer := otel.Tracer("github.com/kennguy3n/zk-drive/internal/audit")
	wsCtx, span := tracer.Start(wsCtx, "audit.ArchiveService.archiveWorkspace")
	defer span.End()
	span.SetAttributes(
		attribute.String("audit.archive.workspace_id", workspaceID.String()),
		attribute.Int("audit.archive.workspace_buckets", len(months)),
	)

	// wsProcessed tracks how many of THIS workspace's months we've
	// fully attempted (success or failure) so the timeout branch can
	// correctly attribute the remaining months to the timeout. Using
	// the run-level RunResult.WorkspaceMonthsOK + WorkspaceMonthsFailed
	// counters here would be wrong — those are cumulative across all
	// workspaces, so months processed by a prior workspace would
	// inflate the "processed" count and undercount this workspace's
	// timed-out remainder.
	wsProcessed := 0
	for _, m := range months {
		if err := wsCtx.Err(); err != nil {
			remaining := len(months) - wsProcessed
			result.WorkspaceMonthsFailed += remaining
			result.Errors = append(result.Errors, fmt.Errorf("workspace %s timed out with %d month(s) unprocessed: %w", workspaceID, remaining, err))
			span.RecordError(err)
			span.SetStatus(codes.Error, "workspace timeout")
			return
		}
		rows, bytesUp, err := a.archiveBucket(wsCtx, runID, cutoff, m)
		wsProcessed++
		// archiveBucket may return rows > 0 alongside a non-nil err
		// when a multi-page bucket failed AFTER some pages had been
		// durably committed (S3 PUT + RecordRun + DeleteBatch all
		// succeeded for pages 1..N-1, page N failed). Those rows are
		// real archive activity that already moved to the cold tier
		// and must NOT be lost from RunResult.RowsArchived /
		// BytesUploaded — otherwise operators monitoring
		// zkdrive_audit_archive_rows_total would see a sustained
		// undercount relative to the rows actually moved. The
		// per-bucket metric label distinguishes "ok" (fully
		// finished), "partial" (some rows committed before failure),
		// and "error" (failed before any commit).
		result.RowsArchived += rows
		result.BytesUploaded += bytesUp
		if err != nil {
			result.WorkspaceMonthsFailed++
			result.Errors = append(result.Errors, fmt.Errorf("workspace %s month %s: %w", workspaceID, m.YearMonth, err))
			slog.ErrorContext(wsCtx, "audit archive bucket failed",
				"workspace_id", workspaceID, "year_month", m.YearMonth,
				"partial_rows", rows, "partial_bytes", bytesUp, "err", err)
			if a.metrics != nil {
				label := archiveBucketResultError
				if rows > 0 {
					label = archiveBucketResultPartial
				}
				a.metrics.RecordAuditArchiveBucket(label, rows, bytesUp)
			}
			continue
		}
		result.WorkspaceMonthsOK++
		if a.metrics != nil {
			a.metrics.RecordAuditArchiveBucket(archiveBucketResultOK, rows, bytesUp)
		}
	}
}

// archiveBucket uploads + deletes one (workspace, month) batch. May
// produce multiple JSONL.gz objects when the bucket exceeds
// MaxRowsPerBatch — each upload gets a distinct UUID-suffixed key
// and a separate audit_log_archive_runs row. Returns the total rows
// + uncompressed bytes archived for this bucket so the caller can
// aggregate run-level totals + emit per-bucket metrics.
func (a *ArchiveService) archiveBucket(ctx context.Context, runID uuid.UUID, cutoff time.Time, m WorkspaceAuditMonth) (int, int64, error) {
	tracer := otel.Tracer("github.com/kennguy3n/zk-drive/internal/audit")
	ctx, span := tracer.Start(ctx, "audit.ArchiveService.archiveBucket")
	defer span.End()
	span.SetAttributes(
		attribute.String("audit.archive.workspace_id", m.WorkspaceID.String()),
		attribute.String("audit.archive.year_month", m.YearMonth),
		attribute.Int("audit.archive.expected_rows", m.RowCount),
	)

	after := uuid.Nil
	totalRows := 0
	totalBytes := int64(0)
	for {
		// Capture per-page startedAt BEFORE the fetch so the
		// audit_log_archive_runs.started_at column reflects the
		// actual moment this page's processing began. completedAt
		// is captured below right before RecordRun. Together they
		// give operators a real duration to plot in dashboards
		// ("how long did one S3 PUT + Postgres write take?") rather
		// than the always-zero-duration the single-shared-now would
		// produce.
		startedAt := a.nowFn().UTC()
		entries, err := a.repo.FetchBatch(ctx, m.WorkspaceID, m.YearMonth, cutoff, a.cfg.MaxRowsPerBatch, after)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "fetch batch failed")
			return totalRows, totalBytes, fmt.Errorf("fetch batch: %w", err)
		}
		if len(entries) == 0 {
			break
		}

		body, byteCount, err := encodeJSONLGzip(entries)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "encode jsonl.gz failed")
			return totalRows, totalBytes, fmt.Errorf("encode jsonl.gz: %w", err)
		}

		// batchID identifies THIS upload page uniquely within the
		// (workspace, month) bucket. runID is reused across batches
		// (one run can produce many batches), but the S3 key MUST
		// be per-batch — otherwise a second batch within the same
		// bucket would overwrite the first one in S3 while the
		// first batch's rows had already been deleted from the hot
		// tier (permanent audit data loss). See package docstring
		// "Design" section.
		batchID := uuid.New()
		objectKey := a.buildObjectKey(m.WorkspaceID, m.YearMonth, batchID)

		uploadCtx, cancel := context.WithTimeout(ctx, a.cfg.UploadTimeout)
		err = a.storage.PutObject(uploadCtx, objectKey, "application/gzip", body)
		cancel()
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "s3 upload failed")
			return totalRows, totalBytes, fmt.Errorf("s3 upload: %w", err)
		}

		ids := make([]uuid.UUID, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.ID)
		}

		completedAt := a.nowFn().UTC()
		record := &ArchiveRunRecord{
			RunID:            runID,
			WorkspaceID:      m.WorkspaceID,
			CutoffTime:       cutoff,
			YearMonth:        m.YearMonth,
			ArchiveObjectKey: objectKey,
			RowsArchived:     len(entries),
			BytesUploaded:    byteCount,
			StartedAt:        startedAt,
			CompletedAt:      completedAt,
		}
		if err := a.repo.RecordRun(ctx, record); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "record run failed")
			return totalRows, totalBytes, fmt.Errorf("record run (S3 object %s is orphaned): %w", objectKey, err)
		}

		deleted, err := a.repo.DeleteBatch(ctx, m.WorkspaceID, ids)
		if err != nil {
			// The S3 object exists, the audit_log_archive_runs
			// row exists, but the rows are still in the hot tier.
			// On the next run the same rows enumerate again, get
			// uploaded under a fresh UUID key, and the cycle
			// retries the delete. Total cost: one orphan S3
			// object + one orphan archive_runs row.
			span.RecordError(err)
			span.SetStatus(codes.Error, "delete batch failed")
			return totalRows + len(entries), totalBytes + byteCount, fmt.Errorf("delete batch (S3 + run record both committed): %w", err)
		}
		if deleted != len(ids) {
			slog.WarnContext(ctx, "audit archive: delete row-count mismatch",
				"expected", len(ids), "deleted", deleted,
				"workspace_id", m.WorkspaceID, "year_month", m.YearMonth)
		}

		totalRows += len(entries)
		totalBytes += byteCount
		after = entries[len(entries)-1].ID

		// If the batch was smaller than the cap we know there
		// are no more rows in this bucket — short-circuit the
		// FetchBatch round-trip.
		if len(entries) < a.cfg.MaxRowsPerBatch {
			break
		}
	}

	span.SetAttributes(
		attribute.Int("audit.archive.rows_archived", totalRows),
		attribute.Int64("audit.archive.bytes_uploaded", totalBytes),
	)
	return totalRows, totalBytes, nil
}

// buildObjectKey assembles the cold-tier S3 key. The format is
// stable so the restore tool can list a workspace's full archive
// history with a single prefix scan:
//
//	{prefix}{workspace_id}/{year-month}/{batch_id}.jsonl.gz
//
// batch_id is a fresh UUID generated PER S3 upload, not per
// archiver invocation. This is load-bearing: a single (workspace,
// month) bucket whose row count exceeds MaxRowsPerBatch produces
// multiple pages, and each page MUST land at a distinct S3 key —
// otherwise the second page would silently overwrite the first
// while the first page's rows had already been deleted from the
// hot tier (permanent audit data loss). The per-invocation runID
// is preserved on the audit_log_archive_runs record so an operator
// can correlate all batches from one tick via WHERE run_id = ?.
//
// When a previous run failed AFTER S3 upload but BEFORE the
// DELETE+RecordRun transaction, the next run sees the same rows
// and uploads them under a NEW batch_id, producing a duplicate
// object. The restore tool dedupes by row id at read time.
func (a *ArchiveService) buildObjectKey(workspaceID uuid.UUID, yearMonth string, batchID uuid.UUID) string {
	return fmt.Sprintf("%s%s/%s/%s.jsonl.gz",
		a.cfg.ArchivePrefix,
		workspaceID.String(),
		yearMonth,
		batchID.String(),
	)
}

// encodeJSONLGzip serialises entries as newline-delimited JSON,
// gzip-compresses the result, and returns the compressed bytes
// alongside the uncompressed-byte count. The (uncompressed bytes
// count is logged on success so the operator dashboard can plot
// raw audit volume independent of compression efficiency.
func encodeJSONLGzip(entries []*Entry) ([]byte, int64, error) {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return nil, 0, fmt.Errorf("encode audit entry %s: %w", e.ID, err)
		}
	}
	uncompressed := int64(raw.Len())

	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	if _, err := io.Copy(gz, &raw); err != nil {
		return nil, 0, fmt.Errorf("gzip write: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, 0, fmt.Errorf("gzip close: %w", err)
	}
	return out.Bytes(), uncompressed, nil
}
