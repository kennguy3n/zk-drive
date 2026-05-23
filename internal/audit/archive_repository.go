package audit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WorkspaceAuditMonth pairs a workspace with one of its calendar
// months that has audit rows eligible for archival. The archiver
// loops over these tuples and processes one (workspace, month) batch
// per pass — keeping the cold-tier key layout aligned with the
// natural restore query ("show me workspace X's audit log for
// March 2024") and bounding per-batch memory by a single workspace-
// month's worth of rows.
type WorkspaceAuditMonth struct {
	WorkspaceID uuid.UUID
	YearMonth   string // canonical "YYYY-MM" form
	RowCount    int
}

// ArchiveRunRecord mirrors one audit_log_archive_runs row. Mirrors
// the table schema directly so the restore tool can re-hydrate it
// without going through the live audit.Entry shape.
type ArchiveRunRecord struct {
	ID               uuid.UUID
	RunID            uuid.UUID
	WorkspaceID      uuid.UUID
	CutoffTime       time.Time
	YearMonth        string
	ArchiveObjectKey string
	RowsArchived     int
	// BytesUploaded is the UNCOMPRESSED JSONL payload size in bytes
	// (the byte count fed into the gzip writer), NOT the on-the-wire
	// size of the gzipped object that landed in S3. The matching
	// Prometheus metric zkdrive_audit_archive_bytes_total carries the
	// same semantic. Operators sizing S3 cost should multiply by the
	// empirically-observed gzip ratio for their audit traffic (≈10x
	// compression in practice). See WS-23 PR #68 Devin Review
	// finding ANALYSIS_pr-review-job-ad89da4c3a1449c5b914d6045dc4ffb8_0003.
	BytesUploaded int64
	StartedAt     time.Time
	CompletedAt   time.Time
	ErrorMessage  *string
}

// ArchiveRepository extends Repository with the workspace-iteration,
// month-bucketing, batched fetch, and post-upload delete operations
// the audit-archiver needs. Kept as a separate interface so the
// archive code path doesn't pollute the existing Repository surface
// (Log, List) used by the request hot path.
type ArchiveRepository interface {
	// EnumerateWorkspaceMonths returns every (workspace, month)
	// tuple with at least one audit_log row older than cutoff.
	// The slice is sorted by workspace_id, year_month so two
	// concurrent archiver invocations process the same buckets in
	// the same order (one will see zero rows on the second pass
	// because the first has already uploaded + deleted).
	EnumerateWorkspaceMonths(ctx context.Context, cutoff time.Time) ([]WorkspaceAuditMonth, error)

	// FetchBatch reads up to limit audit_log rows for the given
	// workspace and month older than cutoff, ordered by id ASC
	// for stable pagination. The id-ordered cursor (vs created_at)
	// avoids the rare case where two rows share the same
	// created_at and get re-fetched on subsequent calls.
	FetchBatch(ctx context.Context, workspaceID uuid.UUID, yearMonth string, cutoff time.Time, limit int, after uuid.UUID) ([]*Entry, error)

	// DeleteBatch removes the named audit_log rows. The query
	// re-asserts workspace_id and id-list match so a stale id
	// list (e.g. a row already deleted by a concurrent run)
	// cannot delete a different workspace's data. Returns the
	// number of rows actually deleted.
	DeleteBatch(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) (int, error)

	// RecordRun INSERTs an audit_log_archive_runs row. Called
	// once per successful (workspace, month) batch. The id
	// column is populated by the database default if record.ID
	// is uuid.Nil.
	RecordRun(ctx context.Context, record *ArchiveRunRecord) error

	// ListRuns returns every archive-run record for the given
	// workspace, newest first. Used by the audit-restore CLI to
	// discover the cold-tier object keys for a workspace's
	// historical period and by future admin-console views.
	ListRuns(ctx context.Context, workspaceID uuid.UUID) ([]*ArchiveRunRecord, error)
}

// PostgresArchiveRepository implements ArchiveRepository against the
// real Postgres pool used by the audit-archiver binary.
type PostgresArchiveRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresArchiveRepository returns a PostgresArchiveRepository
// backed by the supplied pool.
func NewPostgresArchiveRepository(pool *pgxpool.Pool) *PostgresArchiveRepository {
	return &PostgresArchiveRepository{pool: pool}
}

// EnumerateWorkspaceMonths returns every (workspace, year-month)
// tuple with audit rows older than cutoff. The to_char expression
// formats date_trunc('month', created_at) as "YYYY-MM" so the
// archiver can pass the literal string back through FetchBatch
// without re-parsing dates on its side.
func (r *PostgresArchiveRepository) EnumerateWorkspaceMonths(ctx context.Context, cutoff time.Time) ([]WorkspaceAuditMonth, error) {
	const q = `
SELECT workspace_id,
       to_char(date_trunc('month', created_at), 'YYYY-MM') AS year_month,
       COUNT(*) AS row_count
FROM audit_log
WHERE created_at < $1
GROUP BY workspace_id, date_trunc('month', created_at)
ORDER BY workspace_id, year_month`
	rows, err := r.pool.Query(ctx, q, cutoff)
	if err != nil {
		return nil, fmt.Errorf("enumerate audit months: %w", err)
	}
	defer rows.Close()
	var out []WorkspaceAuditMonth
	for rows.Next() {
		var m WorkspaceAuditMonth
		if err := rows.Scan(&m.WorkspaceID, &m.YearMonth, &m.RowCount); err != nil {
			return nil, fmt.Errorf("scan audit month: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FetchBatch pulls a single page of audit_log rows. The page is
// bounded by limit and starts strictly after the supplied cursor
// (uuid.Nil yields the first page).
//
// The month predicate is expressed as a [monthStart, monthEnd)
// half-open timestamp range so the (workspace_id, created_at DESC)
// index covers the entire WHERE clause as a single range scan. A
// previous shape used to_char(date_trunc('month', created_at),
// 'YYYY-MM') = $3, which forced the planner to evaluate the
// function per candidate row and prevented index use for the
// month predicate — a slow path on workspaces with millions of
// audit rows. The id ORDER BY is satisfied by the primary key.
func (r *PostgresArchiveRepository) FetchBatch(
	ctx context.Context,
	workspaceID uuid.UUID,
	yearMonth string,
	cutoff time.Time,
	limit int,
	after uuid.UUID,
) ([]*Entry, error) {
	if limit <= 0 {
		return nil, errors.New("audit archive: fetch limit must be positive")
	}
	monthStart, monthEnd, err := parseYearMonthRange(yearMonth)
	if err != nil {
		return nil, fmt.Errorf("fetch audit batch: %w", err)
	}
	const q = `
SELECT ` + auditColumns + `
FROM audit_log
WHERE workspace_id = $1
  AND created_at < $2
  AND created_at >= $3
  AND created_at < $4
  AND id > $5
ORDER BY id ASC
LIMIT $6`
	rows, err := r.pool.Query(ctx, q, workspaceID, cutoff, monthStart, monthEnd, after, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch audit batch: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// parseYearMonthRange converts a canonical "YYYY-MM" string into
// the half-open [monthStart, monthEnd) UTC timestamp range that
// fully covers it. Used by FetchBatch (and the fake repository in
// archive_test.go) so the production SQL and the test fake apply
// the same month-boundary semantics. Returns an error if the
// string is malformed — EnumerateWorkspaceMonths produces well-
// formed values, so this error path indicates a programming bug.
func parseYearMonthRange(yearMonth string) (time.Time, time.Time, error) {
	t, err := time.ParseInLocation("2006-01", yearMonth, time.UTC)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parse year-month %q: %w", yearMonth, err)
	}
	monthStart := t.UTC()
	monthEnd := monthStart.AddDate(0, 1, 0)
	return monthStart, monthEnd, nil
}

// DeleteBatch removes the supplied ids. The workspace_id predicate
// is defence-in-depth — a malformed id list from the caller cannot
// reach rows outside the workspace the archiver was operating on.
// Returns the number of rows actually deleted (which may be less
// than len(ids) if a concurrent run already deleted some).
func (r *PostgresArchiveRepository) DeleteBatch(ctx context.Context, workspaceID uuid.UUID, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	const q = `DELETE FROM audit_log WHERE workspace_id = $1 AND id = ANY($2)`
	tag, err := r.pool.Exec(ctx, q, workspaceID, ids)
	if err != nil {
		return 0, fmt.Errorf("delete audit batch: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordRun persists one audit_log_archive_runs row. Called after
// the S3 upload completes; the DELETE happens inside the same
// caller-owned flow so a crash between upload and delete leaves
// the rows in place for the next sweep (no audit row is lost; the
// cold tier may carry an orphan object that a future reconciliation
// can detect via the missing DB record).
func (r *PostgresArchiveRepository) RecordRun(ctx context.Context, record *ArchiveRunRecord) error {
	if record.ID == uuid.Nil {
		record.ID = uuid.New()
	}
	const q = `
INSERT INTO audit_log_archive_runs (
    id, run_id, workspace_id, cutoff_time, year_month,
    archive_object_key, rows_archived, bytes_uploaded,
    started_at, completed_at, error_message
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`
	_, err := r.pool.Exec(ctx, q,
		record.ID, record.RunID, record.WorkspaceID, record.CutoffTime, record.YearMonth,
		record.ArchiveObjectKey, record.RowsArchived, record.BytesUploaded,
		record.StartedAt, record.CompletedAt, record.ErrorMessage,
	)
	if err != nil {
		return fmt.Errorf("record archive run: %w", err)
	}
	return nil
}

// ListRuns returns every archive run for the workspace ordered by
// completion time DESC so the restore tool can produce a chronological
// listing for an operator. Returns an empty slice (not nil) when the
// workspace has no archive history yet.
func (r *PostgresArchiveRepository) ListRuns(ctx context.Context, workspaceID uuid.UUID) ([]*ArchiveRunRecord, error) {
	const q = `
SELECT id, run_id, workspace_id, cutoff_time, year_month,
       archive_object_key, rows_archived, bytes_uploaded,
       started_at, completed_at, error_message
FROM audit_log_archive_runs
WHERE workspace_id = $1
ORDER BY completed_at DESC`
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list archive runs: %w", err)
	}
	defer rows.Close()
	out := make([]*ArchiveRunRecord, 0)
	for rows.Next() {
		rec := &ArchiveRunRecord{}
		if err := rows.Scan(
			&rec.ID, &rec.RunID, &rec.WorkspaceID, &rec.CutoffTime, &rec.YearMonth,
			&rec.ArchiveObjectKey, &rec.RowsArchived, &rec.BytesUploaded,
			&rec.StartedAt, &rec.CompletedAt, &rec.ErrorMessage,
		); err != nil {
			// Note: pgx.ErrNoRows is returned by QueryRow().Scan(), never
			// by iteration via rows.Next() + rows.Scan(); rows.Next()
			// returns false once exhausted, so a no-rows condition cannot
			// reach this Scan call. Any error here is a real scan failure.
			return nil, fmt.Errorf("scan archive run: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
