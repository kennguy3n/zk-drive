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
	CheckAccessWithInheritance(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error)
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

// CheckAccessWithInheritance reports whether the grantee has at least
// minRole on the resource, considering both direct grants and grants
// inherited from ancestor folders. The resolution rule (per
// ARCHITECTURE.md §7.2) is "most-specific wins": we walk from the
// resource toward the root, and the first level at which any grant
// exists for the grantee determines the effective role. If the closest
// grant meets minRole the call returns true; if the closest grant is
// below minRole we do NOT continue climbing — that's the "explicit
// grant on a child overrides inherited grants" semantics.
//
// resourceType may be "file" or "folder". For a file we look up its
// containing folder_id once and then use the same ancestor walk as the
// folder case. Expired grants (expires_at <= now()) are ignored.
func (r *PostgresRepository) CheckAccessWithInheritance(ctx context.Context, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID, minRole string) (bool, error) {
	if !isValidRole(minRole) {
		return false, fmt.Errorf("invalid min role %q", minRole)
	}
	if !isValidResourceType(resourceType) {
		return false, fmt.Errorf("invalid resource type %q", resourceType)
	}

	// Step 1: direct grants on the resource. If any grant exists at this
	// level, the most-specific rule says this level wins — return true
	// iff some direct grant meets minRole.
	direct, directAny, err := maxRoleForResource(ctx, r.pool, workspaceID, resourceType, resourceID, granteeType, granteeID)
	if err != nil {
		return false, err
	}
	if directAny {
		return roleRank(direct) >= roleRank(minRole), nil
	}

	// Step 2: resolve the starting folder for the ancestor walk. For a
	// file that's the file's folder_id; for a folder that's the folder's
	// own parent_folder_id (the folder itself had no direct grants, per
	// Step 1).
	var startFolder *uuid.UUID
	switch resourceType {
	case ResourceFile:
		var folderID uuid.UUID
		err := r.pool.QueryRow(ctx, `SELECT folder_id FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`,
			workspaceID, resourceID).Scan(&folderID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, fmt.Errorf("lookup file folder: %w", err)
		}
		startFolder = &folderID
	case ResourceFolder:
		var parent *uuid.UUID
		err := r.pool.QueryRow(ctx, `SELECT parent_folder_id FROM folders WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`,
			workspaceID, resourceID).Scan(&parent)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, fmt.Errorf("lookup folder parent: %w", err)
		}
		startFolder = parent
	}
	if startFolder == nil {
		return false, nil
	}

	// Step 3: walk the folder chain in order (closest ancestor first).
	// At each level check for grants. The first level that has any grant
	// decides the outcome.
	const q = `
WITH RECURSIVE ancestors(id, parent_folder_id, depth) AS (
    SELECT id, parent_folder_id, 0
        FROM folders
        WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT f.id, f.parent_folder_id, a.depth + 1
        FROM folders f
        JOIN ancestors a ON f.id = a.parent_folder_id
        WHERE f.workspace_id = $1 AND f.deleted_at IS NULL
)
SELECT a.id, a.depth, p.role
FROM ancestors a
LEFT JOIN permissions p
    ON p.workspace_id = $1
    AND p.resource_type = 'folder'
    AND p.resource_id = a.id
    AND p.grantee_type = $3
    AND p.grantee_id = $4
    AND (p.expires_at IS NULL OR p.expires_at > now())
ORDER BY a.depth ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID, *startFolder, granteeType, granteeID)
	if err != nil {
		return false, fmt.Errorf("walk folder ancestors: %w", err)
	}
	defer rows.Close()

	var (
		currentDepth = -1
		levelMax     int
		levelHasAny  bool
	)
	finalize := func() (bool, bool) {
		if !levelHasAny {
			return false, false
		}
		return levelMax >= roleRank(minRole), true
	}
	for rows.Next() {
		var (
			folderID uuid.UUID
			depth    int
			role     *string
		)
		if err := rows.Scan(&folderID, &depth, &role); err != nil {
			return false, err
		}
		if depth != currentDepth {
			// Finishing the previous depth: if any grants existed there,
			// the most-specific rule locks the outcome in.
			if result, decided := finalize(); decided {
				return result, nil
			}
			currentDepth = depth
			levelMax = 0
			levelHasAny = false
		}
		if role != nil {
			levelHasAny = true
			if rank := roleRank(*role); rank > levelMax {
				levelMax = rank
			}
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if result, decided := finalize(); decided {
		return result, nil
	}
	return false, nil
}

// maxRoleForResource returns the max role rank found among non-expired
// grants for (resource, grantee) at a single level. The hasAny flag
// distinguishes "no grant at all" from "grant present but below every
// ranked role" (which shouldn't happen given CHECK constraints but we
// keep the signal explicit).
func maxRoleForResource(ctx context.Context, pool *pgxpool.Pool, workspaceID uuid.UUID, resourceType string, resourceID uuid.UUID, granteeType string, granteeID uuid.UUID) (role string, hasAny bool, err error) {
	const q = `
SELECT role FROM permissions
WHERE workspace_id = $1
  AND resource_type = $2 AND resource_id = $3
  AND grantee_type = $4 AND grantee_id = $5
  AND (expires_at IS NULL OR expires_at > now())`
	rows, qerr := pool.Query(ctx, q, workspaceID, resourceType, resourceID, granteeType, granteeID)
	if qerr != nil {
		return "", false, fmt.Errorf("check access: %w", qerr)
	}
	defer rows.Close()
	var bestRank int
	var bestRole string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return "", false, err
		}
		hasAny = true
		if rank := roleRank(r); rank > bestRank {
			bestRank = rank
			bestRole = r
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return bestRole, hasAny, nil
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
