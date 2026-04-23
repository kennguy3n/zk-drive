package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a workspace lookup finds no row.
var ErrNotFound = errors.New("workspace not found")

// Repository defines persistence operations for workspaces.
type Repository interface {
	Create(ctx context.Context, w *Workspace) error
	CreateTx(ctx context.Context, tx pgx.Tx, w *Workspace) error
	GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error)
	Update(ctx context.Context, w *Workspace) error
	ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error)
	SetOwner(ctx context.Context, workspaceID, ownerUserID uuid.UUID) error
	SetOwnerTx(ctx context.Context, tx pgx.Tx, workspaceID, ownerUserID uuid.UUID) error
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const workspaceColumns = "id, name, owner_user_id, storage_quota_bytes, storage_used_bytes, tier, created_at, updated_at"

func scanWorkspace(row pgx.Row) (*Workspace, error) {
	w := &Workspace{}
	if err := row.Scan(&w.ID, &w.Name, &w.OwnerUserID, &w.StorageQuotaBytes, &w.StorageUsedBytes, &w.Tier, &w.CreatedAt, &w.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return w, nil
}

// Create inserts a workspace. Sensible defaults are applied when caller omits
// them.
func (r *PostgresRepository) Create(ctx context.Context, w *Workspace) error {
	return insertWorkspace(ctx, r.pool, w)
}

// CreateTx is the tx-aware equivalent of Create, used by multi-step flows
// that need atomicity (e.g. signup).
func (r *PostgresRepository) CreateTx(ctx context.Context, tx pgx.Tx, w *Workspace) error {
	return insertWorkspace(ctx, tx, w)
}

type workspaceQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertWorkspace(ctx context.Context, q workspaceQuerier, w *Workspace) error {
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	if w.StorageQuotaBytes == 0 {
		w.StorageQuotaBytes = DefaultQuotaBytes
	}
	if w.Tier == "" {
		w.Tier = TierFree
	}
	const stmt = `
INSERT INTO workspaces (id, name, owner_user_id, storage_quota_bytes, storage_used_bytes, tier)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at`
	if err := q.QueryRow(ctx, stmt, w.ID, w.Name, w.OwnerUserID, w.StorageQuotaBytes, w.StorageUsedBytes, w.Tier).
		Scan(&w.CreatedAt, &w.UpdatedAt); err != nil {
		return fmt.Errorf("insert workspace: %w", err)
	}
	return nil
}

// GetByID returns a workspace by its id.
func (r *PostgresRepository) GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	q := "SELECT " + workspaceColumns + " FROM workspaces WHERE id = $1"
	return scanWorkspace(r.pool.QueryRow(ctx, q, id))
}

// Update persists changes to name, tier, and quota fields. CreatedAt is never
// touched.
func (r *PostgresRepository) Update(ctx context.Context, w *Workspace) error {
	const q = `
UPDATE workspaces
SET name = $2, tier = $3, storage_quota_bytes = $4, updated_at = now()
WHERE id = $1
RETURNING updated_at`
	if err := r.pool.QueryRow(ctx, q, w.ID, w.Name, w.Tier, w.StorageQuotaBytes).Scan(&w.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("update workspace: %w", err)
	}
	return nil
}

// SetOwner sets the owner_user_id after the first admin user is created.
func (r *PostgresRepository) SetOwner(ctx context.Context, workspaceID, ownerUserID uuid.UUID) error {
	return setOwner(ctx, r.pool, workspaceID, ownerUserID)
}

// SetOwnerTx is the tx-aware equivalent of SetOwner.
func (r *PostgresRepository) SetOwnerTx(ctx context.Context, tx pgx.Tx, workspaceID, ownerUserID uuid.UUID) error {
	return setOwner(ctx, tx, workspaceID, ownerUserID)
}

func setOwner(ctx context.Context, q workspaceQuerier, workspaceID, ownerUserID uuid.UUID) error {
	tag, err := q.Exec(ctx, `UPDATE workspaces SET owner_user_id = $2, updated_at = now() WHERE id = $1`, workspaceID, ownerUserID)
	if err != nil {
		return fmt.Errorf("set owner: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListForUser returns every workspace the caller belongs to. Because each
// workspace has its own users row per identity, we pivot through the
// caller's email (resolved from the supplied user id) so workspaces joined
// after signup are also returned.
func (r *PostgresRepository) ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error) {
	q := `
SELECT w.id, w.name, w.owner_user_id, w.storage_quota_bytes, w.storage_used_bytes, w.tier, w.created_at, w.updated_at
FROM workspaces w
JOIN users u ON u.workspace_id = w.id
WHERE u.email = (SELECT email FROM users WHERE id = $1)
ORDER BY w.created_at ASC`
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer rows.Close()

	var out []*Workspace
	for rows.Next() {
		w := &Workspace{}
		if err := rows.Scan(&w.ID, &w.Name, &w.OwnerUserID, &w.StorageQuotaBytes, &w.StorageUsedBytes, &w.Tier, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
