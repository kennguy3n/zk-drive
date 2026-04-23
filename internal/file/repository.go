package file

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested file (or version) does not
// exist within the supplied workspace.
var ErrNotFound = errors.New("file not found")

// Repository defines persistence operations for files and file versions.
type Repository interface {
	CreateFile(ctx context.Context, f *File) error
	GetFileByID(ctx context.Context, workspaceID, fileID uuid.UUID) (*File, error)
	UpdateFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string, folderID uuid.UUID) error
	DeleteFile(ctx context.Context, workspaceID, fileID uuid.UUID) error
	MoveFile(ctx context.Context, workspaceID, fileID, folderID uuid.UUID) error
	ListFilesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error)

	CreateFileVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error
	ListVersions(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*FileVersion, error)
	SetCurrentVersion(ctx context.Context, workspaceID, fileID, versionID uuid.UUID) error
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const fileColumns = "id, workspace_id, folder_id, name, current_version_id, size_bytes, mime_type, created_by, created_at, updated_at, deleted_at"

func scanFile(row pgx.Row) (*File, error) {
	f := &File{}
	if err := row.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.CurrentVersionID, &f.SizeBytes, &f.MimeType, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// CreateFile inserts a new file metadata row.
func (r *PostgresRepository) CreateFile(ctx context.Context, f *File) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if f.MimeType == "" {
		f.MimeType = "application/octet-stream"
	}
	const q = `
INSERT INTO files (id, workspace_id, folder_id, name, size_bytes, mime_type, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q, f.ID, f.WorkspaceID, f.FolderID, f.Name, f.SizeBytes, f.MimeType, f.CreatedBy).
		Scan(&f.CreatedAt, &f.UpdatedAt); err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	return nil
}

// GetFileByID returns a non-deleted file within a workspace.
func (r *PostgresRepository) GetFileByID(ctx context.Context, workspaceID, fileID uuid.UUID) (*File, error) {
	q := "SELECT " + fileColumns + " FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	return scanFile(r.pool.QueryRow(ctx, q, workspaceID, fileID))
}

// UpdateFile renames a file and (optionally) moves it to a new folder in a
// single statement.
func (r *PostgresRepository) UpdateFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string, folderID uuid.UUID) error {
	const q = `
UPDATE files SET name = $3, folder_id = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, name, folderID)
	if err != nil {
		return fmt.Errorf("update file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFile soft-deletes a file by setting deleted_at.
func (r *PostgresRepository) DeleteFile(ctx context.Context, workspaceID, fileID uuid.UUID) error {
	const q = `
UPDATE files SET deleted_at = now(), updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveFile updates the folder_id of a file.
func (r *PostgresRepository) MoveFile(ctx context.Context, workspaceID, fileID, folderID uuid.UUID) error {
	const q = `
UPDATE files SET folder_id = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, folderID)
	if err != nil {
		return fmt.Errorf("move file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFilesByFolder returns non-deleted files inside a folder.
func (r *PostgresRepository) ListFilesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error) {
	q := "SELECT " + fileColumns + " FROM files WHERE workspace_id = $1 AND folder_id = $2 AND deleted_at IS NULL ORDER BY name ASC"
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f := &File{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.CurrentVersionID, &f.SizeBytes, &f.MimeType, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateFileVersion inserts a new version row. The caller must ensure the
// file belongs to the given workspace.
func (r *PostgresRepository) CreateFileVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	// Verify workspace ownership and compute next version number atomically.
	var existing int
	if err := r.pool.QueryRow(ctx, `SELECT 1 FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`, workspaceID, v.FileID).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("verify file ownership: %w", err)
	}

	if v.VersionNumber == 0 {
		row := r.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_number), 0) + 1 FROM file_versions WHERE file_id = $1`, v.FileID)
		if err := row.Scan(&v.VersionNumber); err != nil {
			return fmt.Errorf("next version number: %w", err)
		}
	}

	const q = `
INSERT INTO file_versions (id, file_id, version_number, object_key, size_bytes, checksum, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q, v.ID, v.FileID, v.VersionNumber, v.ObjectKey, v.SizeBytes, v.Checksum, v.CreatedBy).
		Scan(&v.CreatedAt); err != nil {
		return fmt.Errorf("insert file version: %w", err)
	}
	return nil
}

// ListVersions returns every version of a file, newest first.
func (r *PostgresRepository) ListVersions(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*FileVersion, error) {
	// Ownership check: the file must belong to the workspace.
	var exists int
	if err := r.pool.QueryRow(ctx, `SELECT 1 FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`, workspaceID, fileID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("verify file workspace: %w", err)
	}
	const q = `
SELECT id, file_id, version_number, object_key, size_bytes, checksum, created_by, created_at
FROM file_versions WHERE file_id = $1 ORDER BY version_number DESC`
	rows, err := r.pool.Query(ctx, q, fileID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	var out []*FileVersion
	for rows.Next() {
		v := &FileVersion{}
		if err := rows.Scan(&v.ID, &v.FileID, &v.VersionNumber, &v.ObjectKey, &v.SizeBytes, &v.Checksum, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetCurrentVersion points a file at one of its existing versions.
func (r *PostgresRepository) SetCurrentVersion(ctx context.Context, workspaceID, fileID, versionID uuid.UUID) error {
	const q = `
UPDATE files SET current_version_id = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, versionID)
	if err != nil {
		return fmt.Errorf("set current version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
