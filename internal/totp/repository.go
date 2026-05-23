package totp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)



// Repository defines persistence for the credential row and the
// recovery-code rows. All reads are scoped by user_id (which is
// itself workspace-scoped via the users foreign key) so tenant
// isolation is enforced at the application layer; the migration
// docstring explains why RLS is not used on these tables.
//
// FinalizeEnrollment is collapsed into a single repository method
// (rather than the service composing three calls inside a tx) so
// the transactional boundary lives in the Postgres-specific layer.
// Test fakes don't have to fake pgx.Tx; production correctness is
// still guaranteed because the activate + wipe + insert sequence
// commits atomically.
type Repository interface {
	// GetCredential returns the user's credential row or
	// ErrNotEnrolled when no row exists.
	GetCredential(ctx context.Context, userID uuid.UUID) (*Credential, error)
	// UpsertCredential overwrites any existing row for the user.
	// Used by BeginEnrollment to replace a stale pending row with
	// a fresh one (so the user can restart enrollment cleanly).
	UpsertCredential(ctx context.Context, cred *Credential) error
	// FinalizeEnrollment is the atomic transition out of pending
	// state. It activates the credential row (sets activated_at),
	// wipes any pre-existing recovery codes for the user, and
	// inserts the supplied bcrypt-hashed new codes — all in a
	// single transaction so a partial failure never leaves a user
	// with a finalized credential but no recovery codes (or vice
	// versa).
	FinalizeEnrollment(ctx context.Context, userID uuid.UUID, activatedAt time.Time, recoveryHashes []string) error
	// DeleteCredential removes the user's row entirely (used by
	// Disable). Cascades via FK to recovery codes.
	DeleteCredential(ctx context.Context, userID uuid.UUID) error
	// UpdateLastUsed stamps the most recently accepted period
	// boundary. Called from a successful Verify.
	UpdateLastUsed(ctx context.Context, userID uuid.UUID, at time.Time) error

	// ListUnusedRecoveryCodes returns un-used recovery code rows
	// for the user. Bounded at len <= 10 by the
	// FinalizeEnrollment contract.
	ListUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]*RecoveryCode, error)
	// MarkRecoveryCodeUsed stamps used_at on a specific recovery
	// code row. Used by ConsumeRecoveryCode after the bcrypt
	// CompareHashAndPassword match succeeds.
	MarkRecoveryCodeUsed(ctx context.Context, id uuid.UUID, at time.Time) error
	// CountUnusedRecoveryCodes is the cheap path for the Status
	// endpoint — no rows are returned, just a count.
	CountUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) (int, error)
}

// PostgresRepository is a pgx-backed implementation of Repository.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a pgx-backed Repository.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const credentialColumns = "user_id, encrypted_secret, activated_at, last_used_at, created_at, updated_at"

func scanCredential(row pgx.Row) (*Credential, error) {
	c := &Credential{}
	if err := row.Scan(
		&c.UserID, &c.EncryptedSecret, &c.ActivatedAt, &c.LastUsedAt,
		&c.CreatedAt, &c.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotEnrolled
		}
		return nil, err
	}
	return c, nil
}

// GetCredential reads the user's credential row.
func (r *PostgresRepository) GetCredential(ctx context.Context, userID uuid.UUID) (*Credential, error) {
	q := "SELECT " + credentialColumns + " FROM user_totp_credentials WHERE user_id = $1"
	return scanCredential(r.pool.QueryRow(ctx, q, userID))
}

// UpsertCredential inserts a new row or replaces an existing one in
// place. Used by BeginEnrollment, which intentionally overwrites a
// stale pending row so a user who clicks "begin enrollment" twice
// gets the most recent secret rather than a confusing mix.
func (r *PostgresRepository) UpsertCredential(ctx context.Context, cred *Credential) error {
	const stmt = `
INSERT INTO user_totp_credentials (user_id, encrypted_secret, activated_at, last_used_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
SET encrypted_secret = EXCLUDED.encrypted_secret,
    activated_at     = EXCLUDED.activated_at,
    last_used_at     = EXCLUDED.last_used_at,
    updated_at       = now()
RETURNING created_at, updated_at`
	if err := r.pool.QueryRow(ctx, stmt,
		cred.UserID, cred.EncryptedSecret, cred.ActivatedAt, cred.LastUsedAt,
	).Scan(&cred.CreatedAt, &cred.UpdatedAt); err != nil {
		return fmt.Errorf("upsert totp credential: %w", err)
	}
	return nil
}

// FinalizeEnrollment activates the pending credential and replaces
// the user's recovery codes atomically. Caller supplies the
// bcrypt-hashed codes; the plaintext never reaches this layer.
func (r *PostgresRepository) FinalizeEnrollment(ctx context.Context, userID uuid.UUID, activatedAt time.Time, recoveryHashes []string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin finalize tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx,
		"UPDATE user_totp_credentials SET activated_at = $2, updated_at = now() WHERE user_id = $1",
		userID, activatedAt,
	)
	if err != nil {
		return fmt.Errorf("activate totp credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnrolled
	}

	if _, err := tx.Exec(ctx,
		"DELETE FROM user_totp_recovery_codes WHERE user_id = $1",
		userID,
	); err != nil {
		return fmt.Errorf("wipe prior recovery codes: %w", err)
	}

	if len(recoveryHashes) > 0 {
		batch := &pgx.Batch{}
		for _, h := range recoveryHashes {
			batch.Queue(
				"INSERT INTO user_totp_recovery_codes (user_id, code_hash) VALUES ($1, $2)",
				userID, h,
			)
		}
		br := tx.SendBatch(ctx, batch)
		for range recoveryHashes {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("insert recovery code: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("close recovery code batch: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit finalize: %w", err)
	}
	return nil
}

// DeleteCredential removes the user's row. ON DELETE CASCADE on the
// recovery-codes table fans out automatically.
func (r *PostgresRepository) DeleteCredential(ctx context.Context, userID uuid.UUID) error {
	tag, err := r.pool.Exec(ctx,
		"DELETE FROM user_totp_credentials WHERE user_id = $1",
		userID,
	)
	if err != nil {
		return fmt.Errorf("delete totp credential: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnrolled
	}
	return nil
}

// UpdateLastUsed stamps the last successful Verify period boundary.
// Best-effort: a failure here is logged by the caller but does not
// invalidate the just-completed login. The next Verify will simply
// not enforce replay protection against the missed period — an
// extremely narrow window the attacker would need to hit precisely.
func (r *PostgresRepository) UpdateLastUsed(ctx context.Context, userID uuid.UUID, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE user_totp_credentials SET last_used_at = $2, updated_at = now() WHERE user_id = $1",
		userID, at,
	)
	if err != nil {
		return fmt.Errorf("update last_used_at: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotEnrolled
	}
	return nil
}

// ListUnusedRecoveryCodes returns un-used codes for the user. The
// partial index idx_user_totp_recovery_codes_unused keeps the scan
// tiny (<=10 rows by design).
func (r *PostgresRepository) ListUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) ([]*RecoveryCode, error) {
	q := "SELECT id, user_id, code_hash, used_at, created_at FROM user_totp_recovery_codes WHERE user_id = $1 AND used_at IS NULL"
	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list unused recovery codes: %w", err)
	}
	defer rows.Close()
	var out []*RecoveryCode
	for rows.Next() {
		c := &RecoveryCode{}
		if err := rows.Scan(&c.ID, &c.UserID, &c.CodeHash, &c.UsedAt, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan recovery code: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// MarkRecoveryCodeUsed stamps used_at on the row.
func (r *PostgresRepository) MarkRecoveryCodeUsed(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.pool.Exec(ctx,
		"UPDATE user_totp_recovery_codes SET used_at = $2 WHERE id = $1 AND used_at IS NULL",
		id, at,
	)
	if err != nil {
		return fmt.Errorf("mark recovery code used: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Treat the race as benign: another concurrent verify
		// already burned this code. The caller has the plaintext
		// match, but we won't accept it twice.
		return ErrCodeReplayed
	}
	return nil
}

// CountUnusedRecoveryCodes is the cheap path for the Status endpoint.
func (r *PostgresRepository) CountUnusedRecoveryCodes(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	if err := r.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM user_totp_recovery_codes WHERE user_id = $1 AND used_at IS NULL",
		userID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("count unused recovery codes: %w", err)
	}
	return n, nil
}


