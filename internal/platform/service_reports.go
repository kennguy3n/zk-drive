package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/zk-drive/internal/billing"
)

// workspaceSummarySelect is the projection shared by ListWorkspaces and
// GetWorkspace. The correlated subqueries keep the join fan-out flat
// (one row per workspace) rather than multiplying by user count.
const workspaceSummarySelect = `
SELECT
    w.id,
    w.name,
    COALESCE(p.tier, w.tier) AS tier,
    (SELECT count(*) FROM users u WHERE u.workspace_id = w.id AND u.deactivated_at IS NULL) AS user_count,
    w.storage_used_bytes,
    w.storage_quota_bytes,
    COALESCE(w.provisioned_by, 'manual') AS provisioned_by,
    w.suspended_at,
    w.suspension_reason,
    (SELECT max(u2.last_login_at) FROM users u2 WHERE u2.workspace_id = w.id) AS last_active_at,
    w.created_at
FROM workspaces w
LEFT JOIN workspace_plans p ON p.workspace_id = w.id`

// storagePercentExpr computes used/quota*100, guarding divide-by-zero.
const storagePercentExpr = `CASE WHEN w.storage_quota_bytes > 0 THEN w.storage_used_bytes::float8 / w.storage_quota_bytes * 100 ELSE 0 END`

// ListWorkspaces returns a filtered, paginated page of workspace
// summaries plus the total count matching the filters (ignoring
// pagination) so the caller can render "showing X of N".
func (s *PlatformService) ListWorkspaces(ctx context.Context, filters ListFilters) ([]WorkspaceSummary, int, error) {
	where, args := buildWorkspaceWhere(filters)

	// Total count first (same predicate, no pagination).
	countSQL := `SELECT count(*) FROM workspaces w LEFT JOIN workspace_plans p ON p.workspace_id = w.id` + where
	var total int
	if err := s.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("platform: count workspaces: %w", err)
	}

	limit := filters.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	offset := filters.Offset
	if offset < 0 {
		offset = 0
	}

	pageArgs := append([]any{}, args...)
	pageArgs = append(pageArgs, limit, offset)
	listSQL := workspaceSummarySelect + where +
		fmt.Sprintf(" ORDER BY w.created_at DESC LIMIT $%d OFFSET $%d", len(pageArgs)-1, len(pageArgs))

	rows, err := s.pool.Query(ctx, listSQL, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("platform: list workspaces: %w", err)
	}
	defer rows.Close()

	out := make([]WorkspaceSummary, 0, limit)
	for rows.Next() {
		sum, err := scanWorkspaceSummary(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("platform: iterate workspaces: %w", err)
	}
	return out, total, nil
}

// GetWorkspace returns the summary for a single workspace.
func (s *PlatformService) GetWorkspace(ctx context.Context, workspaceID uuid.UUID) (*WorkspaceSummary, error) {
	row := s.pool.QueryRow(ctx, workspaceSummarySelect+" WHERE w.id = $1", workspaceID)
	sum, err := scanWorkspaceSummary(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sum, nil
}

// rowScanner abstracts pgx.Row / pgx.Rows so scanWorkspaceSummary
// serves both the single-row and iteration paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorkspaceSummary(row rowScanner) (WorkspaceSummary, error) {
	var (
		sum         WorkspaceSummary
		suspendedAt *time.Time
		reason      *string
		lastActive  *time.Time
	)
	if err := row.Scan(
		&sum.ID, &sum.Name, &sum.Tier, &sum.UserCount,
		&sum.StorageUsedBytes, &sum.StorageQuotaBytes, &sum.ProvisionedBy,
		&suspendedAt, &reason, &lastActive, &sum.CreatedAt,
	); err != nil {
		return WorkspaceSummary{}, err
	}
	sum.SuspendedAt = suspendedAt
	sum.Suspended = suspendedAt != nil
	if reason != nil {
		sum.SuspensionReason = *reason
	}
	sum.LastActiveAt = lastActive
	sum.StoragePercent = storagePercent(sum.StorageUsedBytes, sum.StorageQuotaBytes)
	return sum, nil
}

// buildWorkspaceWhere assembles the WHERE clause and positional args
// shared by the count and list queries. It returns an empty string
// when no filters apply.
func buildWorkspaceWhere(f ListFilters) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	add := func(expr string, val any) {
		args = append(args, val)
		clauses = append(clauses, fmt.Sprintf(expr, len(args)))
	}
	if t := strings.TrimSpace(f.Tier); t != "" {
		add("COALESCE(p.tier, w.tier) = $%d", t)
	}
	if f.Suspended != nil {
		if *f.Suspended {
			clauses = append(clauses, "w.suspended_at IS NOT NULL")
		} else {
			clauses = append(clauses, "w.suspended_at IS NULL")
		}
	}
	if f.MinStoragePercent > 0 {
		add(storagePercentExpr+" >= $%d", f.MinStoragePercent)
	}
	if f.MaxStoragePercent > 0 {
		add(storagePercentExpr+" <= $%d", f.MaxStoragePercent)
	}
	if f.CreatedAfter != nil {
		add("w.created_at >= $%d", *f.CreatedAfter)
	}
	if f.CreatedBefore != nil {
		add("w.created_at <= $%d", *f.CreatedBefore)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// GetWorkspaceUsage returns the detailed per-workspace usage report.
func (s *PlatformService) GetWorkspaceUsage(ctx context.Context, workspaceID uuid.UUID) (*UsageReport, error) {
	var (
		tier       string
		usedBytes  int64
		quotaBytes int64
	)
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(p.tier, w.tier), w.storage_used_bytes, w.storage_quota_bytes
         FROM workspaces w
         LEFT JOIN workspace_plans p ON p.workspace_id = w.id
         WHERE w.id = $1`,
		workspaceID,
	).Scan(&tier, &usedBytes, &quotaBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("platform: load workspace usage: %w", err)
	}

	report := &UsageReport{
		WorkspaceID:       workspaceID,
		Tier:              tier,
		StorageUsedBytes:  usedBytes,
		StorageQuotaBytes: quotaBytes,
		StoragePercent:    storagePercent(usedBytes, quotaBytes),
		GeneratedAt:       s.now(),
	}

	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE workspace_id = $1 AND deleted_at IS NULL`,
		workspaceID,
	).Scan(&report.FileCount); err != nil {
		return nil, fmt.Errorf("platform: count files: %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_previews fp JOIN files f ON f.id = fp.file_id WHERE f.workspace_id = $1`,
		workspaceID,
	).Scan(&report.PreviewCount); err != nil {
		return nil, fmt.Errorf("platform: count previews: %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM usage_events
         WHERE workspace_id = $1 AND event_type = $2 AND created_at >= date_trunc('month', now())`,
		workspaceID, billing.EventBandwidth,
	).Scan(&report.BandwidthMTDBytes); err != nil {
		return nil, fmt.Errorf("platform: sum bandwidth: %w", err)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE workspace_id = $1 AND deactivated_at IS NULL`,
		workspaceID,
	).Scan(&report.UserCount); err != nil {
		return nil, fmt.Errorf("platform: count users: %w", err)
	}

	// Resolved bandwidth limit comes from the billing plan (falling
	// back to the tier defaults when no override row exists).
	if limits, _, lerr := s.billing.LimitsFor(ctx, workspaceID); lerr == nil {
		report.BandwidthLimitBytes = limits.MaxBandwidthBytesMonthly
	} else {
		s.log().Warn("platform: resolve limits failed", "workspace_id", workspaceID, "err", lerr)
	}
	return report, nil
}

// storagePercent computes used/quota*100, returning 0 when quota is
// non-positive (unlimited or unset).
func storagePercent(used, quota int64) float64 {
	if quota <= 0 {
		return 0
	}
	return float64(used) / float64(quota) * 100
}
