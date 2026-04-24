package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a requested user does not exist.
var ErrNotFound = errors.New("user not found")

// Repository defines persistence operations for users. All reads are scoped
// to a workspace id to enforce tenant isolation at the data layer.
type Repository interface {
	Create(ctx context.Context, u *User) error
	CreateTx(ctx context.Context, tx pgx.Tx, u *User) error
	GetByID(ctx context.Context, workspaceID, userID uuid.UUID) (*User, error)
	GetByEmail(ctx context.Context, workspaceID uuid.UUID, email string) (*User, error)
	GetByEmailAnyWorkspace(ctx context.Context, email string) (*User, error)
	GetByAuthProvider(ctx context.Context, provider, providerID string) (*User, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]*User, error)
	UpdateLastLogin(ctx context.Context, userID uuid.UUID, at time.Time) error
	Deactivate(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time) error
	UpdateRole(ctx context.Context, workspaceID, userID uuid.UUID, role string) error
	LinkAuthProvider(ctx context.Context, userID uuid.UUID, provider, providerID string) error
}

// PostgresRepository is a pgx-backed implementation of Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a new PostgresRepository using the given pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const userColumns = "id, workspace_id, email, name, password_hash, role, auth_provider, auth_provider_id, last_login_at, deactivated_at, created_at, updated_at"

func scanUser(row pgx.Row) (*User, error) {
	u := &User{}
	if err := row.Scan(
		&u.ID, &u.WorkspaceID, &u.Email, &u.Name, &u.PasswordHash, &u.Role,
		&u.AuthProvider, &u.AuthProviderID, &u.LastLoginAt, &u.DeactivatedAt,
		&u.CreatedAt, &u.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return u, nil
}

// Create inserts a new user row. The user's ID is populated in-place.
func (r *PostgresRepository) Create(ctx context.Context, u *User) error {
	return insertUser(ctx, r.pool, u)
}

// CreateTx is the tx-aware equivalent of Create, used by multi-step flows
// (signup, add-user-to-workspace) that need atomicity.
func (r *PostgresRepository) CreateTx(ctx context.Context, tx pgx.Tx, u *User) error {
	return insertUser(ctx, tx, u)
}

type userQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func insertUser(ctx context.Context, q userQuerier, u *User) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	if u.Role == "" {
		u.Role = RoleMember
	}
	const stmt = `
INSERT INTO users (id, workspace_id, email, name, password_hash, role)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING created_at, updated_at`
	if err := q.QueryRow(ctx, stmt, u.ID, u.WorkspaceID, u.Email, u.Name, u.PasswordHash, u.Role).
		Scan(&u.CreatedAt, &u.UpdatedAt); err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// GetByID fetches a user by id, scoped to a workspace.
func (r *PostgresRepository) GetByID(ctx context.Context, workspaceID, userID uuid.UUID) (*User, error) {
	q := "SELECT " + userColumns + " FROM users WHERE workspace_id = $1 AND id = $2"
	return scanUser(r.pool.QueryRow(ctx, q, workspaceID, userID))
}

// GetByEmail fetches a user by workspace + email combination.
func (r *PostgresRepository) GetByEmail(ctx context.Context, workspaceID uuid.UUID, email string) (*User, error) {
	q := "SELECT " + userColumns + " FROM users WHERE workspace_id = $1 AND email = $2"
	return scanUser(r.pool.QueryRow(ctx, q, workspaceID, email))
}

// GetByEmailAnyWorkspace returns the oldest user row with the given email.
// Ordering by created_at guarantees the same (email) -> user mapping across
// logins when a user belongs to multiple workspaces; callers that need a
// specific workspace should pass workspace_id on the login request.
func (r *PostgresRepository) GetByEmailAnyWorkspace(ctx context.Context, email string) (*User, error) {
	q := "SELECT " + userColumns + " FROM users WHERE email = $1 ORDER BY created_at ASC, id ASC LIMIT 1"
	return scanUser(r.pool.QueryRow(ctx, q, email))
}

// GetByAuthProvider returns a user row matched on (auth_provider,
// auth_provider_id). Used by the SSO callback to resolve the caller to
// an existing account without requiring the email to be stable (some
// identity providers let users change emails).
func (r *PostgresRepository) GetByAuthProvider(ctx context.Context, provider, providerID string) (*User, error) {
	q := "SELECT " + userColumns + " FROM users WHERE auth_provider = $1 AND auth_provider_id = $2"
	return scanUser(r.pool.QueryRow(ctx, q, provider, providerID))
}

// List returns every user belonging to a workspace.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID) ([]*User, error) {
	q := "SELECT " + userColumns + " FROM users WHERE workspace_id = $1 ORDER BY created_at ASC"
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(
			&u.ID, &u.WorkspaceID, &u.Email, &u.Name, &u.PasswordHash, &u.Role,
			&u.AuthProvider, &u.AuthProviderID, &u.LastLoginAt, &u.DeactivatedAt,
			&u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateLastLogin stamps the user's last_login_at column. Called on
// every successful login (password or SSO).
func (r *PostgresRepository) UpdateLastLogin(ctx context.Context, userID uuid.UUID, at time.Time) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at = $2, updated_at = now() WHERE id = $1`, userID, at)
	if err != nil {
		return fmt.Errorf("update last login: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Deactivate soft-deactivates a user by setting deactivated_at. The
// row is preserved so audit history still resolves the actor.
func (r *PostgresRepository) Deactivate(ctx context.Context, workspaceID, userID uuid.UUID, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET deactivated_at = $3, updated_at = now() WHERE workspace_id = $1 AND id = $2 AND deactivated_at IS NULL`,
		workspaceID, userID, at)
	if err != nil {
		return fmt.Errorf("deactivate user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateRole changes a user's role within a workspace.
func (r *PostgresRepository) UpdateRole(ctx context.Context, workspaceID, userID uuid.UUID, role string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET role = $3, updated_at = now() WHERE workspace_id = $1 AND id = $2`,
		workspaceID, userID, role)
	if err != nil {
		return fmt.Errorf("update role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkAuthProvider stamps an existing user with the given provider +
// subject id. Called from the SSO callback when a password user logs
// in via Google/Microsoft for the first time.
func (r *PostgresRepository) LinkAuthProvider(ctx context.Context, userID uuid.UUID, provider, providerID string) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET auth_provider = $2, auth_provider_id = $3, updated_at = now() WHERE id = $1`,
		userID, provider, providerID)
	if err != nil {
		return fmt.Errorf("link auth provider: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
