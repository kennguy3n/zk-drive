package retention

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a policy id does not resolve.
var ErrNotFound = errors.New("retention policy not found")

// Repository defines persistence operations for retention_policies.
type Repository interface {
	List(ctx context.Context, workspaceID uuid.UUID) ([]*Policy, error)
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*Policy, error)
	Upsert(ctx context.Context, p *Policy) error
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// PostgresRepository is a pgx-backed Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository backed by pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const retentionCols = "id, workspace_id, folder_id, max_versions, max_age_days, archive_after_days, created_by, created_at, updated_at"

func scanPolicy(row pgx.Row) (*Policy, error) {
	p := &Policy{}
	if err := row.Scan(
		&p.ID, &p.WorkspaceID, &p.FolderID,
		&p.MaxVersions, &p.MaxAgeDays, &p.ArchiveAfterDays,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// List returns all policies scoped to a workspace.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID) ([]*Policy, error) {
	q := "SELECT " + retentionCols + " FROM retention_policies WHERE workspace_id = $1 ORDER BY created_at ASC"
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list policies: %w", err)
	}
	defer rows.Close()
	var out []*Policy
	for rows.Next() {
		p := &Policy{}
		if err := rows.Scan(
			&p.ID, &p.WorkspaceID, &p.FolderID,
			&p.MaxVersions, &p.MaxAgeDays, &p.ArchiveAfterDays,
			&p.CreatedBy, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get fetches a single policy by id, workspace-scoped.
func (r *PostgresRepository) Get(ctx context.Context, workspaceID, id uuid.UUID) (*Policy, error) {
	q := "SELECT " + retentionCols + " FROM retention_policies WHERE workspace_id = $1 AND id = $2"
	return scanPolicy(r.pool.QueryRow(ctx, q, workspaceID, id))
}

// Upsert inserts or updates a policy keyed on (workspace_id,
// COALESCE(folder_id, sentinel)). The unique partial index on that
// expression is enforced in migration 012.
func (r *PostgresRepository) Upsert(ctx context.Context, p *Policy) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	const q = `
INSERT INTO retention_policies
    (id, workspace_id, folder_id, max_versions, max_age_days, archive_after_days, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id, COALESCE(folder_id, '00000000-0000-0000-0000-000000000000'::uuid))
DO UPDATE SET
    max_versions       = EXCLUDED.max_versions,
    max_age_days       = EXCLUDED.max_age_days,
    archive_after_days = EXCLUDED.archive_after_days,
    updated_at         = now()
RETURNING id, created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q,
		p.ID, p.WorkspaceID, p.FolderID,
		p.MaxVersions, p.MaxAgeDays, p.ArchiveAfterDays,
		p.CreatedBy,
	).Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return fmt.Errorf("upsert policy: %w", err)
	}
	return nil
}

// Delete removes a policy by id, workspace-scoped.
func (r *PostgresRepository) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM retention_policies WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
