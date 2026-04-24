package sharing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when the requested share link or guest invite
// does not exist (or has been deleted).
var ErrNotFound = errors.New("sharing: not found")

// Repository defines persistence operations for share_links and
// guest_invites. All queries filter by workspace_id except for
// GetShareLinkByToken, which is the unauthenticated public entry point;
// the token itself is UNIQUE across workspaces and encodes enough
// entropy that cross-tenant guessing is infeasible, and the link
// metadata carries its own workspace_id for downstream scoping.
type Repository interface {
	CreateShareLink(ctx context.Context, link *ShareLink) error
	GetShareLinkByToken(ctx context.Context, token string) (*ShareLink, error)
	GetShareLinkByID(ctx context.Context, workspaceID, id uuid.UUID) (*ShareLink, error)
	DeleteShareLink(ctx context.Context, workspaceID, id uuid.UUID) error
	IncrementDownloadCount(ctx context.Context, id uuid.UUID) error

	CreateGuestInvite(ctx context.Context, invite *GuestInvite) error
	GetGuestInviteByID(ctx context.Context, workspaceID, id uuid.UUID) (*GuestInvite, error)
	ListGuestInvitesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*GuestInvite, error)
	AcceptGuestInvite(ctx context.Context, workspaceID, id uuid.UUID, acceptedAt time.Time) error
	DeleteGuestInvite(ctx context.Context, workspaceID, id uuid.UUID) error
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

const shareLinkColumns = "id, workspace_id, resource_type, resource_id, token, password_hash, expires_at, max_downloads, download_count, created_by, created_at"

func scanShareLink(row pgx.Row) (*ShareLink, error) {
	l := &ShareLink{}
	if err := row.Scan(
		&l.ID, &l.WorkspaceID, &l.ResourceType, &l.ResourceID,
		&l.Token, &l.PasswordHash, &l.ExpiresAt, &l.MaxDownloads,
		&l.DownloadCount, &l.CreatedBy, &l.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return l, nil
}

// CreateShareLink inserts a share_links row. ID is populated in-place
// when not already set.
func (r *PostgresRepository) CreateShareLink(ctx context.Context, link *ShareLink) error {
	if link.ID == uuid.Nil {
		link.ID = uuid.New()
	}
	const q = `
INSERT INTO share_links
    (id, workspace_id, resource_type, resource_id, token, password_hash, expires_at, max_downloads, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING download_count, created_at`
	if err := r.pool.QueryRow(ctx, q,
		link.ID, link.WorkspaceID, link.ResourceType, link.ResourceID,
		link.Token, link.PasswordHash, link.ExpiresAt, link.MaxDownloads, link.CreatedBy,
	).Scan(&link.DownloadCount, &link.CreatedAt); err != nil {
		return fmt.Errorf("insert share link: %w", err)
	}
	return nil
}

// GetShareLinkByToken resolves a share link by its public token. The
// caller is responsible for checking expiry / password / download cap.
func (r *PostgresRepository) GetShareLinkByToken(ctx context.Context, token string) (*ShareLink, error) {
	q := "SELECT " + shareLinkColumns + " FROM share_links WHERE token = $1"
	return scanShareLink(r.pool.QueryRow(ctx, q, token))
}

// GetShareLinkByID fetches a share link by id scoped to workspace.
func (r *PostgresRepository) GetShareLinkByID(ctx context.Context, workspaceID, id uuid.UUID) (*ShareLink, error) {
	q := "SELECT " + shareLinkColumns + " FROM share_links WHERE workspace_id = $1 AND id = $2"
	return scanShareLink(r.pool.QueryRow(ctx, q, workspaceID, id))
}

// DeleteShareLink revokes a share link. Returns ErrNotFound when no row
// matched the workspace/id pair.
func (r *PostgresRepository) DeleteShareLink(ctx context.Context, workspaceID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM share_links WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete share link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IncrementDownloadCount atomically bumps download_count by 1 iff
// max_downloads is either unset or has not yet been reached. This
// closes the TOCTOU race window that existed when callers checked
// link.IsExhausted() on a cached snapshot before incrementing — two
// concurrent resolutions on a link with max_downloads=1 could both
// pass the check and both increment, handing out an extra download.
//
// The UPDATE uses a boolean guard inside WHERE so the check and the
// increment happen in a single SQL statement. A zero RowsAffected()
// result can mean either "row does not exist" or "row exists but cap
// already reached"; we disambiguate with a follow-up SELECT so the
// caller can distinguish ErrNotFound from ErrLinkExhausted.
func (r *PostgresRepository) IncrementDownloadCount(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE share_links SET download_count = download_count + 1
		 WHERE id = $1 AND (max_downloads IS NULL OR download_count < max_downloads)`,
		id,
	)
	if err != nil {
		return fmt.Errorf("increment download count: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Disambiguate between missing row and exhausted cap. Any other
	// error propagates unchanged so the caller sees the true failure.
	var exists bool
	if serr := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM share_links WHERE id = $1)`, id).Scan(&exists); serr != nil {
		return fmt.Errorf("disambiguate increment failure: %w", serr)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrLinkExhausted
}

const guestInviteColumns = "id, workspace_id, email, folder_id, role, expires_at, accepted_at, permission_id, created_by, created_at"

func scanGuestInvite(row pgx.Row) (*GuestInvite, error) {
	g := &GuestInvite{}
	if err := row.Scan(
		&g.ID, &g.WorkspaceID, &g.Email, &g.FolderID, &g.Role,
		&g.ExpiresAt, &g.AcceptedAt, &g.PermissionID, &g.CreatedBy, &g.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return g, nil
}

// CreateGuestInvite inserts a guest_invites row. ID is populated in-place
// when not already set.
func (r *PostgresRepository) CreateGuestInvite(ctx context.Context, invite *GuestInvite) error {
	if invite.ID == uuid.Nil {
		invite.ID = uuid.New()
	}
	const q = `
INSERT INTO guest_invites
    (id, workspace_id, email, folder_id, role, expires_at, permission_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`
	if err := r.pool.QueryRow(ctx, q,
		invite.ID, invite.WorkspaceID, invite.Email, invite.FolderID,
		invite.Role, invite.ExpiresAt, invite.PermissionID, invite.CreatedBy,
	).Scan(&invite.CreatedAt); err != nil {
		return fmt.Errorf("insert guest invite: %w", err)
	}
	return nil
}

// GetGuestInviteByID fetches an invite scoped to workspace.
func (r *PostgresRepository) GetGuestInviteByID(ctx context.Context, workspaceID, id uuid.UUID) (*GuestInvite, error) {
	q := "SELECT " + guestInviteColumns + " FROM guest_invites WHERE workspace_id = $1 AND id = $2"
	return scanGuestInvite(r.pool.QueryRow(ctx, q, workspaceID, id))
}

// ListGuestInvitesByFolder returns every invite targeting a folder in a
// workspace, newest first.
func (r *PostgresRepository) ListGuestInvitesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*GuestInvite, error) {
	q := "SELECT " + guestInviteColumns + ` FROM guest_invites
WHERE workspace_id = $1 AND folder_id = $2
ORDER BY created_at DESC`
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list guest invites: %w", err)
	}
	defer rows.Close()
	var out []*GuestInvite
	for rows.Next() {
		g, err := scanGuestInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// AcceptGuestInvite marks an invite accepted at the supplied timestamp.
// Returns ErrNotFound if no matching row was found.
func (r *PostgresRepository) AcceptGuestInvite(ctx context.Context, workspaceID, id uuid.UUID, acceptedAt time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE guest_invites SET accepted_at = $3 WHERE workspace_id = $1 AND id = $2 AND accepted_at IS NULL`,
		workspaceID, id, acceptedAt)
	if err != nil {
		return fmt.Errorf("accept guest invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteGuestInvite revokes an invite. The associated permission row (if
// any) is deleted separately by the service layer so we keep the two
// concerns composable.
func (r *PostgresRepository) DeleteGuestInvite(ctx context.Context, workspaceID, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM guest_invites WHERE workspace_id = $1 AND id = $2`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("delete guest invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
