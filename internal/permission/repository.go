package permission

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested permission does not exist.
var ErrNotFound = errors.New("permission not found")

// Repository defines persistence operations for permissions. Every method
// filters on workspace_id so a compromised or buggy caller cannot reach
// rows belonging to another tenant.
type Repository interface {
	Create(ctx context.Context, p *Permission) error
	GetByID(ctx context.Context, workspaceID, permID uuid.UUID) (*Permission, error)
	ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]*Permission, error)
	ListByGrantee(ctx context.Context, workspaceID uuid.UUID, granteeType string, granteeID uuid.UUID) ([]*Permission, error)
	Delete(ctx context.Context, workspaceID, permID uuid.UUID) error
	CheckAccess(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const permColumns = "id, workspace_id, resource_type, resource_id, grantee_type, grantee_id, role, created_at, expires_at"

func scanPermission(row pgx.Row) (*Permission, error) {
	p := &Permission{}
	if err := row.Scan(
		&p.ID, &p.WorkspaceID, &p.ResourceType, &p.ResourceID,
		&p.GranteeType, &p.GranteeID, &p.Role, &p.CreatedAt, &p.ExpiresAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// Create inserts a permission row. The ID is populated in-place.
func (r *PostgresRepository) Create(ctx context.Context, p *Permission) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	const q = `
INSERT INTO permissions (id, workspace_id, resource_type, resource_id, grantee_type, grantee_id, role, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q,
		p.ID, p.WorkspaceID, p.ResourceType, p.ResourceID,
		p.GranteeType, p.GranteeID, p.Role, p.ExpiresAt,
	).Scan(&p.CreatedAt); err != nil {
		return fmt.Errorf("insert permission: %w", err)
	}
	return nil
}

// GetByID fetches a permission scoped to a workspace.
func (r *PostgresRepository) GetByID(ctx context.Context, workspaceID, permID uuid.UUID) (*Permission, error) {
	q := "SELECT " + permColumns + " FROM permissions WHERE workspace_id = $1 AND id = $2"
	return scanPermission(r.pool.QueryRow(ctx, q, workspaceID, permID))
}

// ListByResource returns every grant on a given resource within a workspace.
func (r *PostgresRepository) ListByResource(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID) ([]*Permission, error) {
	q := "SELECT " + permColumns + ` FROM permissions
WHERE workspace_id = $1 AND resource_type = $2 AND resource_id = $3
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID, resourceType, resourceID)
	if err != nil {
		return nil, fmt.Errorf("list by resource: %w", err)
	}
	defer rows.Close()
	return scanPermissionRows(rows)
}

// ListByGrantee returns every grant issued to a given grantee within a
// workspace.
func (r *PostgresRepository) ListByGrantee(ctx context.Context, workspaceID uuid.UUID, granteeType string, granteeID uuid.UUID) ([]*Permission, error) {
	q := "SELECT " + permColumns + ` FROM permissions
WHERE workspace_id = $1 AND grantee_type = $2 AND grantee_id = $3
ORDER BY created_at ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID, granteeType, granteeID)
	if err != nil {
		return nil, fmt.Errorf("list by grantee: %w", err)
	}
	defer rows.Close()
	return scanPermissionRows(rows)
}

// Delete removes a permission grant. Returns ErrNotFound if no row matched.
func (r *PostgresRepository) Delete(ctx context.Context, workspaceID, permID uuid.UUID) error {
	const q = `DELETE FROM permissions WHERE workspace_id = $1 AND id = $2`
	tag, err := r.pool.Exec(ctx, q, workspaceID, permID)
	if err != nil {
		return fmt.Errorf("delete permission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// CheckAccess reports whether the grantee has at least minRole on the
// resource. Role hierarchy: admin > editor > viewer. Expired grants
// (expires_at <= now()) are ignored. Phase 1 does not inherit from parent
// folders — that lands in Phase 2.
func (r *PostgresRepository) CheckAccess(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	if !isValidRole(minRole) {
		return false, fmt.Errorf("invalid min role %q", minRole)
	}
	const q = `
SELECT role FROM permissions
WHERE workspace_id = $1
  AND resource_type = $2 AND resource_id = $3
  AND grantee_type = $4 AND grantee_id = $5
  AND (expires_at IS NULL OR expires_at > now())`
	rows, err := r.pool.Query(ctx, q, workspaceID, resourceType, resourceID, granteeType, granteeID)
	if err != nil {
		return false, fmt.Errorf("check access: %w", err)
	}
	defer rows.Close()
	minRank := roleRank(minRole)
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return false, err
		}
		if roleRank(role) >= minRank {
			return true, nil
		}
	}
	return false, rows.Err()
}

func scanPermissionRows(rows pgx.Rows) ([]*Permission, error) {
	var out []*Permission
	for rows.Next() {
		p := &Permission{}
		if err := rows.Scan(
			&p.ID, &p.WorkspaceID, &p.ResourceType, &p.ResourceID,
			&p.GranteeType, &p.GranteeID, &p.Role, &p.CreatedAt, &p.ExpiresAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
