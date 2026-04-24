package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository defines persistence operations for audit_log.
type Repository interface {
	Log(ctx context.Context, entry *Entry) error
	List(ctx context.Context, workspaceID uuid.UUID, action string, limit, offset int) ([]*Entry, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const auditColumns = "id, workspace_id, actor_id, action, resource_type, resource_id, host(ip_address), user_agent, metadata, created_at"

// Log inserts an audit_log row synchronously. Callers should route
// writes through Service, which wraps this with a background worker.
func (r *PostgresRepository) Log(ctx context.Context, entry *Entry) error {
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	const q = `
INSERT INTO audit_log (id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7::inet, $8, $9)
RETURNING created_at`
	var metadata any
	if len(entry.Metadata) > 0 {
		metadata = []byte(entry.Metadata)
	}
	var ip any
	if entry.IPAddress != nil && *entry.IPAddress != "" {
		ip = *entry.IPAddress
	}
	if err := r.pool.QueryRow(ctx, q,
		entry.ID, entry.WorkspaceID, entry.ActorID, entry.Action,
		entry.ResourceType, entry.ResourceID, ip, entry.UserAgent, metadata,
	).Scan(&entry.CreatedAt); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// List returns paginated workspace audit entries, newest first. When
// action is non-empty the result is filtered by the action column.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID, action string, limit, offset int) ([]*Entry, error) {
	limit, offset = normalizePaging(limit, offset)
	var rows pgx.Rows
	var err error
	if action == "" {
		q := "SELECT " + auditColumns + ` FROM audit_log
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`
		rows, err = r.pool.Query(ctx, q, workspaceID, limit, offset)
	} else {
		q := "SELECT " + auditColumns + ` FROM audit_log
WHERE workspace_id = $1 AND action = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4`
		rows, err = r.pool.Query(ctx, q, workspaceID, action, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows pgx.Rows) ([]*Entry, error) {
	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		var (
			metadata  []byte
			ipAddress *string
			userAgent *string
		)
		if err := rows.Scan(
			&e.ID, &e.WorkspaceID, &e.ActorID, &e.Action,
			&e.ResourceType, &e.ResourceID, &ipAddress, &userAgent, &metadata, &e.CreatedAt,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return out, nil
			}
			return nil, err
		}
		if ipAddress != nil {
			e.IPAddress = ipAddress
		}
		if userAgent != nil {
			e.UserAgent = userAgent
		}
		if len(metadata) > 0 {
			e.Metadata = metadata
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
