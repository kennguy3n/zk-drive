package sharing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// clientRoomColumns is factored out so every SELECT uses the same
// ordering and a single scanClientRoom can parse any row.
const clientRoomColumns = "id, workspace_id, name, folder_id, share_link_id, dropbox_enabled, expires_at, created_by, created_at"

func scanClientRoom(row pgx.Row) (*ClientRoom, error) {
	r := &ClientRoom{}
	if err := row.Scan(
		&r.ID, &r.WorkspaceID, &r.Name, &r.FolderID, &r.ShareLinkID,
		&r.DropboxEnabled, &r.ExpiresAt, &r.CreatedBy, &r.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return r, nil
}

// PostgresClientRoomRepository is the Postgres implementation of
// ClientRoomRepository.
type PostgresClientRoomRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresClientRoomRepository returns a Postgres-backed
// ClientRoomRepository.
func NewPostgresClientRoomRepository(pool *pgxpool.Pool) *PostgresClientRoomRepository {
	return &PostgresClientRoomRepository{pool: pool}
}

// Create inserts a client_rooms row. ID is populated in-place when the
// caller did not pre-assign one.
func (r *PostgresClientRoomRepository) Create(ctx context.Context, room *ClientRoom) error {
	if room.ID == uuid.Nil {
		room.ID = uuid.New()
	}
	const q = `
INSERT INTO client_rooms
    (id, workspace_id, name, folder_id, share_link_id, dropbox_enabled, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q,
		room.ID, room.WorkspaceID, room.Name, room.FolderID,
		room.ShareLinkID, room.DropboxEnabled, room.ExpiresAt, room.CreatedBy,
	).Scan(&room.CreatedAt); err != nil {
		return fmt.Errorf("insert client room: %w", err)
	}
	return nil
}

// Get fetches a room scoped to workspace.
func (r *PostgresClientRoomRepository) Get(ctx context.Context, workspaceID, id uuid.UUID) (*ClientRoom, error) {
	q := "SELECT " + clientRoomColumns + " FROM client_rooms WHERE workspace_id = $1 AND id = $2"
	return scanClientRoom(r.pool.QueryRow(ctx, q, workspaceID, id))
}

// List returns every room in a workspace, newest first.
func (r *PostgresClientRoomRepository) List(ctx context.Context, workspaceID uuid.UUID) ([]*ClientRoom, error) {
	q := "SELECT " + clientRoomColumns + " FROM client_rooms WHERE workspace_id = $1 ORDER BY created_at DESC"
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list client rooms: %w", err)
	}
	defer rows.Close()
	var out []*ClientRoom
	for rows.Next() {
		room, err := scanClientRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, room)
	}
	return out, rows.Err()
}

// Delete removes the room row. The backing folder and share link are
// managed by the service layer (share link is revoked first so the
// public URL stops working immediately).
func (r *PostgresClientRoomRepository) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM client_rooms WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete client room: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
