package notification

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested notification does not
// exist in the caller's workspace.
var ErrNotFound = errors.New("notification: not found")

// Repository defines persistence operations for notifications.
type Repository interface {
	Create(ctx context.Context, n *Notification) error
	ListForUser(ctx context.Context, workspaceID, userID uuid.UUID, limit, offset int) ([]*Notification, error)
	MarkRead(ctx context.Context, workspaceID, userID, id uuid.UUID) error
	MarkAllRead(ctx context.Context, workspaceID, userID uuid.UUID) error
	ListWorkspaceAdmins(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const notificationColumns = "id, workspace_id, user_id, type, title, body, resource_type, resource_id, read_at, created_at"

func scanNotification(row pgx.Row) (*Notification, error) {
	n := &Notification{}
	if err := row.Scan(
		&n.ID, &n.WorkspaceID, &n.UserID, &n.Type, &n.Title, &n.Body,
		&n.ResourceType, &n.ResourceID, &n.ReadAt, &n.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return n, nil
}

// Create inserts a notification row. ID is populated in-place.
func (r *PostgresRepository) Create(ctx context.Context, n *Notification) error {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	const q = `
INSERT INTO notifications
    (id, workspace_id, user_id, type, title, body, resource_type, resource_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q,
		n.ID, n.WorkspaceID, n.UserID, n.Type, n.Title, n.Body, n.ResourceType, n.ResourceID,
	).Scan(&n.CreatedAt); err != nil {
		return fmt.Errorf("insert notification: %w", err)
	}
	return nil
}

// ListForUser returns notifications for a user in a workspace, unread
// first then by newest. Callers paginate via limit / offset; both are
// clamped by the caller.
func (r *PostgresRepository) ListForUser(ctx context.Context, workspaceID, userID uuid.UUID, limit, offset int) ([]*Notification, error) {
	q := "SELECT " + notificationColumns + ` FROM notifications
WHERE workspace_id = $1 AND user_id = $2
ORDER BY (read_at IS NULL) DESC, created_at DESC
LIMIT $3 OFFSET $4`
	rows, err := r.pool.Query(ctx, q, workspaceID, userID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	var out []*Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkRead flips a single notification to read. Already-read rows are
// left alone (idempotent).
func (r *PostgresRepository) MarkRead(ctx context.Context, workspaceID, userID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE notifications
         SET read_at = now()
         WHERE workspace_id = $1 AND user_id = $2 AND id = $3 AND read_at IS NULL`,
		workspaceID, userID, id)
	if err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Could be already-read (fine) or not-found (error). Probe.
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM notifications WHERE workspace_id = $1 AND user_id = $2 AND id = $3)`, workspaceID, userID, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// MarkAllRead flips every unread notification for the user.
func (r *PostgresRepository) MarkAllRead(ctx context.Context, workspaceID, userID uuid.UUID) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE notifications
         SET read_at = now()
         WHERE workspace_id = $1 AND user_id = $2 AND read_at IS NULL`,
		workspaceID, userID)
	if err != nil {
		return fmt.Errorf("mark all read: %w", err)
	}
	return nil
}

// ListWorkspaceAdmins returns the user_ids of all admins in a
// workspace. Used when fanning out admin-scoped notifications (e.g.
// scan quarantines).
func (r *PostgresRepository) ListWorkspaceAdmins(ctx context.Context, workspaceID uuid.UUID) ([]uuid.UUID, error) {
	// workspace membership lives on the `users` table (see migration
	// 001): each user has a single workspace_id and a role. We treat
	// role='admin' as the recipient set for admin-scoped events.
	rows, err := r.pool.Query(ctx,
		`SELECT id FROM users
         WHERE workspace_id = $1 AND role = 'admin'`,
		workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list workspace admins: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
