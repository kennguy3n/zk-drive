package kchat

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// roomColumns is factored out so every SELECT uses the same ordering
// and a single scanRoom can parse any row.
const roomColumns = "id, workspace_id, kchat_room_id, folder_id, created_by, created_at"

func scanRoom(row pgx.Row) (*RoomFolder, error) {
	r := &RoomFolder{}
	if err := row.Scan(
		&r.ID, &r.WorkspaceID, &r.KChatRoomID, &r.FolderID,
		&r.CreatedBy, &r.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRoomNotFound
		}
		return nil, err
	}
	return r, nil
}

// Repository is the persistence surface used by RoomService. Every
// method is workspace-scoped so the service layer can never
// accidentally leak rows across tenants.
type Repository interface {
	Create(ctx context.Context, r *RoomFolder) error
	Get(ctx context.Context, workspaceID, id uuid.UUID) (*RoomFolder, error)
	GetByRoomID(ctx context.Context, workspaceID uuid.UUID, kchatRoomID string) (*RoomFolder, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*RoomFolder, error)
	Delete(ctx context.Context, workspaceID, id uuid.UUID) error
}

// PostgresRepository is the Postgres-backed implementation of
// Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wires a Postgres-backed Repository.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// uniqueViolation reports whether err is a unique-constraint
// violation. Used by Create to translate the (workspace, room)
// uniqueness constraint into ErrRoomAlreadyMapped.
func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// Create inserts a kchat_room_folders row. ID is populated in-place
// when the caller did not pre-assign one. A unique-constraint
// violation on (workspace_id, kchat_room_id) is translated into
// ErrRoomAlreadyMapped so the service layer can surface 409 Conflict.
func (r *PostgresRepository) Create(ctx context.Context, room *RoomFolder) error {
	if room.ID == uuid.Nil {
		room.ID = uuid.New()
	}
	const q = `
INSERT INTO kchat_room_folders
    (id, workspace_id, kchat_room_id, folder_id, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q,
		room.ID, room.WorkspaceID, strings.TrimSpace(room.KChatRoomID),
		room.FolderID, room.CreatedBy,
	).Scan(&room.CreatedAt); err != nil {
		if uniqueViolation(err) {
			return ErrRoomAlreadyMapped
		}
		return fmt.Errorf("insert kchat room folder: %w", err)
	}
	return nil
}

// Get fetches a mapping by id, scoped to workspace.
func (r *PostgresRepository) Get(ctx context.Context, workspaceID, id uuid.UUID) (*RoomFolder, error) {
	q := "SELECT " + roomColumns + " FROM kchat_room_folders WHERE workspace_id = $1 AND id = $2"
	return scanRoom(r.pool.QueryRow(ctx, q, workspaceID, id))
}

// GetByRoomID fetches a mapping by the KChat-side room identifier.
func (r *PostgresRepository) GetByRoomID(ctx context.Context, workspaceID uuid.UUID, kchatRoomID string) (*RoomFolder, error) {
	q := "SELECT " + roomColumns + " FROM kchat_room_folders WHERE workspace_id = $1 AND kchat_room_id = $2"
	return scanRoom(r.pool.QueryRow(ctx, q, workspaceID, strings.TrimSpace(kchatRoomID)))
}

// List returns every mapping in a workspace, newest first.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID) ([]*RoomFolder, error) {
	q := "SELECT " + roomColumns + " FROM kchat_room_folders WHERE workspace_id = $1 ORDER BY created_at DESC"
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list kchat room folders: %w", err)
	}
	defer rows.Close()
	var out []*RoomFolder
	for rows.Next() {
		room, err := scanRoom(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, room)
	}
	return out, rows.Err()
}

// Delete removes a mapping row by id, scoped to workspace.
func (r *PostgresRepository) Delete(ctx context.Context, workspaceID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM kchat_room_folders WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete kchat room folder: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRoomNotFound
	}
	return nil
}
