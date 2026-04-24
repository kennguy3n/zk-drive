package activity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository defines persistence operations for activity_log. All reads
// filter by workspace_id for tenant isolation.
type Repository interface {
	Log(ctx context.Context, entry *LogEntry) error
	List(ctx context.Context, workspaceID uuid.UUID, limit, offset int) ([]*LogEntry, error)
	ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, limit, offset int) ([]*LogEntry, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const activityColumns = "id, workspace_id, user_id, action, resource_type, resource_id, metadata_json, created_at"

// Log inserts an activity_log row synchronously. The Service wraps this
// with a background worker so callers don't pay the latency cost.
func (r *PostgresRepository) Log(ctx context.Context, entry *LogEntry) error {
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	const q = `
INSERT INTO activity_log (id, workspace_id, user_id, action, resource_type, resource_id, metadata_json)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at`
	var metadata any
	if len(entry.MetadataJSON) > 0 {
		metadata = []byte(entry.MetadataJSON)
	}
	if err := r.pool.QueryRow(ctx, q,
		entry.ID, entry.WorkspaceID, entry.UserID,
		entry.Action, entry.ResourceType, entry.ResourceID, metadata,
	).Scan(&entry.CreatedAt); err != nil {
		return fmt.Errorf("insert activity: %w", err)
	}
	return nil
}

// List returns paginated workspace activity, newest first. A limit <= 0 is
// treated as 50; an offset < 0 is treated as 0.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID, limit, offset int) ([]*LogEntry, error) {
	limit, offset = normalizePaging(limit, offset)
	q := "SELECT " + activityColumns + ` FROM activity_log
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`
	rows, err := r.pool.Query(ctx, q, workspaceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list activity: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ListByResource returns paginated activity rows for a specific resource.
func (r *PostgresRepository) ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, limit, offset int) ([]*LogEntry, error) {
	limit, offset = normalizePaging(limit, offset)
	q := "SELECT " + activityColumns + ` FROM activity_log
WHERE workspace_id = $1 AND resource_type = $2 AND resource_id = $3
ORDER BY created_at DESC
LIMIT $4 OFFSET $5`
	rows, err := r.pool.Query(ctx, q, workspaceID, resourceType, resourceID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list activity by resource: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows pgx.Rows) ([]*LogEntry, error) {
	var out []*LogEntry
	for rows.Next() {
		e := &LogEntry{}
		var metadata []byte
		if err := rows.Scan(
			&e.ID, &e.WorkspaceID, &e.UserID, &e.Action,
			&e.ResourceType, &e.ResourceID, &metadata, &e.CreatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return out, nil
			}
			return nil, err
		}
		if len(metadata) > 0 {
			e.MetadataJSON = metadata
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func normalizePaging(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
