package preview

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when no preview row matches the requested
// file_id / version_id.
var ErrNotFound = errors.New("preview: not found")

// Repository defines persistence operations for file_previews.
type Repository interface {
	Upsert(ctx context.Context, p *Preview) error
	GetByVersion(ctx context.Context, fileID, versionID uuid.UUID) (*Preview, error)
	GetLatestByFile(ctx context.Context, fileID uuid.UUID) (*Preview, error)
}

// PostgresRepository implements Repository against Postgres using a
// pgxpool.Pool.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const previewColumns = "id, file_id, version_id, object_key, mime_type, size_bytes, created_at"

func scanPreview(row pgx.Row) (*Preview, error) {
	p := &Preview{}
	if err := row.Scan(&p.ID, &p.FileID, &p.VersionID, &p.ObjectKey, &p.MimeType, &p.SizeBytes, &p.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// Upsert inserts or replaces a preview row for (file_id, version_id).
// ID is populated in-place on insert and preserved on update.
func (r *PostgresRepository) Upsert(ctx context.Context, p *Preview) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	const q = `
INSERT INTO file_previews (id, file_id, version_id, object_key, mime_type, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (file_id, version_id) DO UPDATE SET
    object_key = EXCLUDED.object_key,
    mime_type  = EXCLUDED.mime_type,
    size_bytes = EXCLUDED.size_bytes
RETURNING id, created_at`
	if err := r.pool.QueryRow(ctx, q,
		p.ID, p.FileID, p.VersionID, p.ObjectKey, p.MimeType, p.SizeBytes,
	).Scan(&p.ID, &p.CreatedAt); err != nil {
		return fmt.Errorf("upsert preview: %w", err)
	}
	return nil
}

// GetByVersion returns the preview row for a specific version.
func (r *PostgresRepository) GetByVersion(ctx context.Context, fileID, versionID uuid.UUID) (*Preview, error) {
	q := "SELECT " + previewColumns + " FROM file_previews WHERE file_id = $1 AND version_id = $2"
	return scanPreview(r.pool.QueryRow(ctx, q, fileID, versionID))
}

// GetLatestByFile returns the most recently generated preview for a
// file. Used by the frontend when it doesn't know the current
// version_id (e.g. rendering a grid of thumbnails).
func (r *PostgresRepository) GetLatestByFile(ctx context.Context, fileID uuid.UUID) (*Preview, error) {
	q := "SELECT " + previewColumns + " FROM file_previews WHERE file_id = $1 ORDER BY created_at DESC LIMIT 1"
	return scanPreview(r.pool.QueryRow(ctx, q, fileID))
}
