package folder

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested folder cannot be located (or has
// been soft-deleted).
var ErrNotFound = errors.New("folder not found")

// Repository defines persistence operations for folders.
type Repository interface {
	Create(ctx context.Context, f *Folder) error
	GetByID(ctx context.Context, workspaceID, folderID uuid.UUID) (*Folder, error)
	UpdateNameAndPath(ctx context.Context, workspaceID, folderID uuid.UUID, name, path string) error
	UpdateParentAndPath(ctx context.Context, workspaceID, folderID uuid.UUID, parentID *uuid.UUID, path string) error
	SoftDelete(ctx context.Context, workspaceID, folderID uuid.UUID) error
	SoftDeleteSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) error
	ListChildren(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID) ([]*Folder, error)
	ListDescendants(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Folder, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const folderColumns = "id, workspace_id, parent_folder_id, name, path, created_by, created_at, updated_at, deleted_at"

func scanFolder(row pgx.Row) (*Folder, error) {
	f := &Folder{}
	if err := row.Scan(&f.ID, &f.WorkspaceID, &f.ParentFolderID, &f.Name, &f.Path, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Create inserts a folder row.
func (r *PostgresRepository) Create(ctx context.Context, f *Folder) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	const q = `
INSERT INTO folders (id, workspace_id, parent_folder_id, name, path, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q, f.ID, f.WorkspaceID, f.ParentFolderID, f.Name, f.Path, f.CreatedBy).
		Scan(&f.CreatedAt, &f.UpdatedAt); err != nil {
		return fmt.Errorf("insert folder: %w", err)
	}
	return nil
}

// GetByID returns a non-deleted folder within a workspace.
func (r *PostgresRepository) GetByID(ctx context.Context, workspaceID, folderID uuid.UUID) (*Folder, error) {
	q := "SELECT " + folderColumns + " FROM folders WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	return scanFolder(r.pool.QueryRow(ctx, q, workspaceID, folderID))
}

// UpdateNameAndPath renames a folder in-place.
func (r *PostgresRepository) UpdateNameAndPath(ctx context.Context, workspaceID, folderID uuid.UUID, name, path string) error {
	const q = `
UPDATE folders SET name = $3, path = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, folderID, name, path)
	if err != nil {
		return fmt.Errorf("rename folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateParentAndPath moves a folder under a new parent.
func (r *PostgresRepository) UpdateParentAndPath(ctx context.Context, workspaceID, folderID uuid.UUID, parentID *uuid.UUID, path string) error {
	const q = `
UPDATE folders SET parent_folder_id = $3, path = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, folderID, parentID, path)
	if err != nil {
		return fmt.Errorf("move folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDelete marks a single folder as deleted.
func (r *PostgresRepository) SoftDelete(ctx context.Context, workspaceID, folderID uuid.UUID) error {
	const q = `
UPDATE folders SET deleted_at = now(), updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, folderID)
	if err != nil {
		return fmt.Errorf("delete folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDeleteSubtree marks the given folder and every descendant folder (plus
// files contained anywhere in the subtree) as soft-deleted.
func (r *PostgresRepository) SoftDeleteSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin subtree delete: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const folderQ = `
WITH RECURSIVE subtree AS (
    SELECT id FROM folders
        WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT f.id FROM folders f
        JOIN subtree s ON f.parent_folder_id = s.id
        WHERE f.workspace_id = $1 AND f.deleted_at IS NULL
)
UPDATE folders SET deleted_at = now(), updated_at = now()
WHERE workspace_id = $1 AND id IN (SELECT id FROM subtree) AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, folderQ, workspaceID, folderID)
	if err != nil {
		return fmt.Errorf("delete folder subtree: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	const fileQ = `
WITH RECURSIVE subtree AS (
    SELECT id FROM folders WHERE workspace_id = $1 AND id = $2
    UNION ALL
    SELECT f.id FROM folders f JOIN subtree s ON f.parent_folder_id = s.id
        WHERE f.workspace_id = $1
)
UPDATE files SET deleted_at = now(), updated_at = now()
WHERE workspace_id = $1 AND folder_id IN (SELECT id FROM subtree) AND deleted_at IS NULL`
	if _, err := tx.Exec(ctx, fileQ, workspaceID, folderID); err != nil {
		return fmt.Errorf("delete files in subtree: %w", err)
	}

	return tx.Commit(ctx)
}

// ListChildren returns non-deleted folders whose parent_folder_id matches.
// Passing a nil parentID lists root folders.
func (r *PostgresRepository) ListChildren(ctx context.Context, workspaceID uuid.UUID, parentID *uuid.UUID) ([]*Folder, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if parentID == nil {
		q := "SELECT " + folderColumns + " FROM folders WHERE workspace_id = $1 AND parent_folder_id IS NULL AND deleted_at IS NULL ORDER BY name ASC"
		rows, err = r.pool.Query(ctx, q, workspaceID)
	} else {
		q := "SELECT " + folderColumns + " FROM folders WHERE workspace_id = $1 AND parent_folder_id = $2 AND deleted_at IS NULL ORDER BY name ASC"
		rows, err = r.pool.Query(ctx, q, workspaceID, *parentID)
	}
	if err != nil {
		return nil, fmt.Errorf("list children: %w", err)
	}
	defer rows.Close()

	var out []*Folder
	for rows.Next() {
		f := &Folder{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.ParentFolderID, &f.Name, &f.Path, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListDescendants returns every non-deleted descendant folder.
func (r *PostgresRepository) ListDescendants(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Folder, error) {
	q := `
WITH RECURSIVE subtree AS (
    SELECT ` + folderColumns + ` FROM folders
        WHERE workspace_id = $1 AND parent_folder_id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT f.id, f.workspace_id, f.parent_folder_id, f.name, f.path, f.created_by, f.created_at, f.updated_at, f.deleted_at
        FROM folders f JOIN subtree s ON f.parent_folder_id = s.id
        WHERE f.workspace_id = $1 AND f.deleted_at IS NULL
)
SELECT ` + folderColumns + ` FROM subtree`
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list descendants: %w", err)
	}
	defer rows.Close()

	var out []*Folder
	for rows.Next() {
		f := &Folder{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.ParentFolderID, &f.Name, &f.Path, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
