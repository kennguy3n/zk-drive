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
	// GetSearchLanguageByID is a hot-path helper used by the
	// search handler on every search request. It pulls JUST the
	// search_language column with a primary-key lookup, avoiding
	// the GetByID full-row scan (which serialises 10 columns the
	// search path doesn't need). At workspace scales of 10K+ this
	// matters: high-QPS search would otherwise pay the full-row
	// scan / unmarshal cost per query. Returns ErrNotFound when no
	// row matches, and the empty string with no error when the
	// column itself is empty (defence in depth against a future
	// migration dropping NOT NULL).
	GetSearchLanguageByID(ctx context.Context, workspaceID uuid.UUID) (string, error)
	Update(ctx context.Context, w *Workspace) error
	ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error)
	SetOwner(ctx context.Context, workspaceID, ownerUserID uuid.UUID) error
	SetOwnerTx(ctx context.Context, tx pgx.Tx, workspaceID, ownerUserID uuid.UUID) error
	// SetMFARequired flips the workspaces.mfa_required column for
	// the admin policy-toggle endpoint. Returns the previous value
	// so the audit log can capture the transition.
	SetMFARequired(ctx context.Context, workspaceID uuid.UUID, required bool) (previous bool, err error)
	// SetSearchLanguage updates the workspace's FTS dictionary
	// for stemming. Returns the previous value so the audit log
	// can record the transition; returns ErrNotFound when no row
	// matches. The caller MUST have already validated lang via
	// IsSupportedSearchLanguage — the repo does not re-validate
	// (single source of truth lives in workspace.go).
	SetSearchLanguage(ctx context.Context, workspaceID uuid.UUID, lang string) (previous string, err error)
	// GetDefaultEncryptionModeByID is a hot-path helper used by the
	// folder-create flow to resolve the workspace's default mode for
	// new root folders. Like GetSearchLanguageByID it pulls JUST the
	// one column with a primary-key lookup rather than the full-row
	// GetByID scan. Returns ErrNotFound when no row matches, and
	// ("", nil) when the column is empty (defence in depth) so the
	// caller can fall back to the package default.
	GetDefaultEncryptionModeByID(ctx context.Context, workspaceID uuid.UUID) (string, error)
	// SetDefaultEncryptionMode updates workspaces.default_encryption_mode.
	// Returns the previous value so the audit log can record the
	// transition; returns ErrNotFound when no row matches. The caller
	// MUST have already validated mode via IsValidDefaultEncryptionMode.
	SetDefaultEncryptionMode(ctx context.Context, workspaceID uuid.UUID, mode string) (previous string, err error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const workspaceColumns = "id, name, owner_user_id, storage_quota_bytes, storage_used_bytes, tier, mfa_required, search_language, default_encryption_mode, created_at, updated_at"

func scanWorkspace(row pgx.Row) (*Workspace, error) {
	w := &Workspace{}
	if err := row.Scan(&w.ID, &w.Name, &w.OwnerUserID, &w.StorageQuotaBytes, &w.StorageUsedBytes, &w.Tier, &w.MFARequired, &w.SearchLanguage, &w.DefaultEncryptionMode, &w.CreatedAt, &w.UpdatedAt); err != nil {
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
	if w.SearchLanguage == "" {
		w.SearchLanguage = DefaultSearchLanguage
	}
	if w.DefaultEncryptionMode == "" {
		w.DefaultEncryptionMode = DefaultEncryptionMode
	}
	// We RETURN search_language / default_encryption_mode (alongside
	// created_at / updated_at) so the in-memory struct mirrors the row
	// Postgres just wrote. Without this, callers that immediately
	// JSON-encode the returned Workspace see "search_language": ""
	// even though the column on disk has the DEFAULT applied — a
	// subtle drift that bites the admin-page-after-create path.
	const stmt = `
INSERT INTO workspaces (id, name, owner_user_id, storage_quota_bytes, storage_used_bytes, tier, search_language, default_encryption_mode)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at, updated_at, search_language, default_encryption_mode`
	if err := q.QueryRow(ctx, stmt, w.ID, w.Name, w.OwnerUserID, w.StorageQuotaBytes, w.StorageUsedBytes, w.Tier, w.SearchLanguage, w.DefaultEncryptionMode).
		Scan(&w.CreatedAt, &w.UpdatedAt, &w.SearchLanguage, &w.DefaultEncryptionMode); err != nil {
		return fmt.Errorf("insert workspace: %w", err)
	}
	return nil
}

// GetByID returns a workspace by its id.
func (r *PostgresRepository) GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	q := "SELECT " + workspaceColumns + " FROM workspaces WHERE id = $1"
	return scanWorkspace(r.pool.QueryRow(ctx, q, id))
}

// GetSearchLanguageByID returns just the search_language column —
// a hot path used by the search handler on every request. A
// dedicated single-column query is cheaper than the GetByID
// full-row scan: at high search QPS the FTS handler does NOT need
// owner_user_id / storage_quota_bytes / mfa_required / etc., so
// pulling the whole row and discarding nine columns is wasted
// bandwidth between Postgres and the API pod. The query still
// hits the same primary-key index as GetByID, so the latency
// difference is small per-request but compounds at scale.
//
// Returns ErrNotFound when no row matches. Returns ("", nil) when
// the row exists but search_language is empty — callers (the
// service layer's GetSearchLanguage helper) map this to
// DefaultSearchLanguage. We deliberately don't apply the default
// at the repo layer so the audit log can see the actual on-disk
// value if that matters.
func (r *PostgresRepository) GetSearchLanguageByID(ctx context.Context, id uuid.UUID) (string, error) {
	const q = "SELECT search_language FROM workspaces WHERE id = $1"
	var lang string
	if err := r.pool.QueryRow(ctx, q, id).Scan(&lang); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read search_language: %w", err)
	}
	return lang, nil
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

// SetMFARequired flips the mfa_required column and returns the
// prior value so the caller can record the transition in the
// audit log. Returns ErrNotFound if no row matches.
func (r *PostgresRepository) SetMFARequired(ctx context.Context, workspaceID uuid.UUID, required bool) (bool, error) {
	// Two-step under an explicit transaction so the previous-value
	// read and the UPDATE happen atomically. Without this, a
	// concurrent policy toggle could read the same prior value
	// twice and misreport one of the two transitions in the audit
	// log.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev bool
	if err := tx.QueryRow(ctx,
		"SELECT mfa_required FROM workspaces WHERE id = $1 FOR UPDATE",
		workspaceID,
	).Scan(&prev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("read mfa_required: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE workspaces SET mfa_required = $2, updated_at = now() WHERE id = $1",
		workspaceID, required,
	); err != nil {
		return false, fmt.Errorf("update mfa_required: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit mfa_required: %w", err)
	}
	return prev, nil
}

// SetSearchLanguage updates the workspace's FTS dictionary under
// a SELECT ... FOR UPDATE / UPDATE pair so a concurrent toggle
// can't misreport the previous value to the audit log. Same
// concurrency reasoning as SetMFARequired.
func (r *PostgresRepository) SetSearchLanguage(ctx context.Context, workspaceID uuid.UUID, lang string) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev string
	if err := tx.QueryRow(ctx,
		"SELECT search_language FROM workspaces WHERE id = $1 FOR UPDATE",
		workspaceID,
	).Scan(&prev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read search_language: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE workspaces SET search_language = $2, updated_at = now() WHERE id = $1",
		workspaceID, lang,
	); err != nil {
		return "", fmt.Errorf("update search_language: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit search_language: %w", err)
	}
	return prev, nil
}

// GetDefaultEncryptionModeByID returns just the
// default_encryption_mode column via a primary-key lookup. The
// folder-create flow calls this for every new root folder, so a
// single-column read is cheaper than the full-row GetByID scan. See
// GetSearchLanguageByID for the same reasoning. Returns ErrNotFound
// when no row matches and ("", nil) when the column is empty.
func (r *PostgresRepository) GetDefaultEncryptionModeByID(ctx context.Context, id uuid.UUID) (string, error) {
	const q = "SELECT default_encryption_mode FROM workspaces WHERE id = $1"
	var mode string
	if err := r.pool.QueryRow(ctx, q, id).Scan(&mode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read default_encryption_mode: %w", err)
	}
	return mode, nil
}

// SetDefaultEncryptionMode updates workspaces.default_encryption_mode
// under a SELECT ... FOR UPDATE / UPDATE pair so a concurrent toggle
// can't misreport the previous value to the audit log. Same
// concurrency reasoning as SetMFARequired / SetSearchLanguage.
func (r *PostgresRepository) SetDefaultEncryptionMode(ctx context.Context, workspaceID uuid.UUID, mode string) (string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var prev string
	if err := tx.QueryRow(ctx,
		"SELECT default_encryption_mode FROM workspaces WHERE id = $1 FOR UPDATE",
		workspaceID,
	).Scan(&prev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read default_encryption_mode: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"UPDATE workspaces SET default_encryption_mode = $2, updated_at = now() WHERE id = $1",
		workspaceID, mode,
	); err != nil {
		return "", fmt.Errorf("update default_encryption_mode: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit default_encryption_mode: %w", err)
	}
	return prev, nil
}

// ListForUser returns every workspace the caller belongs to. Because each
// workspace has its own users row per identity, we pivot through the
// caller's email (resolved from the supplied user id) so workspaces joined
// after signup are also returned.
func (r *PostgresRepository) ListForUser(ctx context.Context, userID uuid.UUID) ([]*Workspace, error) {
	q := `
SELECT w.id, w.name, w.owner_user_id, w.storage_quota_bytes, w.storage_used_bytes, w.tier, w.mfa_required, w.search_language, w.created_at, w.updated_at
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
		if err := rows.Scan(&w.ID, &w.Name, &w.OwnerUserID, &w.StorageQuotaBytes, &w.StorageUsedBytes, &w.Tier, &w.MFARequired, &w.SearchLanguage, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
