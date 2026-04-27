package billing

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the persistence interface for billing. The HTTP
// handlers and the Service compose against this interface so future
// implementations (e.g. Stripe-backed) can swap in without changes
// upstream.
type Repository interface {
	GetPlan(ctx context.Context, workspaceID uuid.UUID) (*Plan, error)
	UpsertPlan(ctx context.Context, p *Plan) (*Plan, error)
	UpdateTier(ctx context.Context, workspaceID uuid.UUID, tier string) (*Plan, error)
	SetStripeCustomerID(ctx context.Context, workspaceID uuid.UUID, customerID string) error
	RecordEvent(ctx context.Context, workspaceID uuid.UUID, eventType string, bytes int64) error
	GetStorageUsed(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	GetBandwidthUsedThisMonth(ctx context.Context, workspaceID uuid.UUID) (int64, error)
	GetUserCount(ctx context.Context, workspaceID uuid.UUID) (int, error)
}

// PostgresRepository implements Repository against pgx.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository wraps pool in a PostgresRepository.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

// GetPlan returns the workspace_plans row for a workspace. Returns
// ErrPlanNotFound when no row exists so the Service can fall back to
// the free-tier defaults.
func (r *PostgresRepository) GetPlan(ctx context.Context, workspaceID uuid.UUID) (*Plan, error) {
	const q = `
SELECT id, workspace_id, tier, max_storage_bytes, max_users,
       max_bandwidth_bytes_monthly, stripe_customer_id, created_at, updated_at
FROM workspace_plans
WHERE workspace_id = $1`
	p := &Plan{}
	err := r.pool.QueryRow(ctx, q, workspaceID).Scan(
		&p.ID, &p.WorkspaceID, &p.Tier,
		&p.MaxStorageBytes, &p.MaxUsers, &p.MaxBandwidthBytesMonthly,
		&p.StripeCustomerID,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPlanNotFound
		}
		return nil, fmt.Errorf("get plan: %w", err)
	}
	return p, nil
}

// UpsertPlan inserts or updates a workspace's plan. Limits set to nil
// on the input clear the per-workspace override (so the per-tier
// default applies again).
func (r *PostgresRepository) UpsertPlan(ctx context.Context, p *Plan) (*Plan, error) {
	// stripe_customer_id is preserved on conflict when the caller
	// passes nil so a webhook-driven tier change doesn't blow away an
	// existing customer linkage. All other override columns mirror
	// the input directly — passing nil is the documented way to
	// clear an override.
	const q = `
INSERT INTO workspace_plans
    (id, workspace_id, tier, max_storage_bytes, max_users, max_bandwidth_bytes_monthly, stripe_customer_id)
VALUES (COALESCE($1, uuid_generate_v4()), $2, $3, $4, $5, $6, $7)
ON CONFLICT (workspace_id) DO UPDATE SET
    tier = EXCLUDED.tier,
    max_storage_bytes = EXCLUDED.max_storage_bytes,
    max_users = EXCLUDED.max_users,
    max_bandwidth_bytes_monthly = EXCLUDED.max_bandwidth_bytes_monthly,
    stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, workspace_plans.stripe_customer_id),
    updated_at = now()
RETURNING id, workspace_id, tier, max_storage_bytes, max_users,
          max_bandwidth_bytes_monthly, stripe_customer_id, created_at, updated_at`
	out := &Plan{}
	var idArg *uuid.UUID
	if p.ID != uuid.Nil {
		idArg = &p.ID
	}
	err := r.pool.QueryRow(ctx, q,
		idArg, p.WorkspaceID, p.Tier,
		p.MaxStorageBytes, p.MaxUsers, p.MaxBandwidthBytesMonthly,
		p.StripeCustomerID,
	).Scan(
		&out.ID, &out.WorkspaceID, &out.Tier,
		&out.MaxStorageBytes, &out.MaxUsers, &out.MaxBandwidthBytesMonthly,
		&out.StripeCustomerID,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert plan: %w", err)
	}
	return out, nil
}

// UpdateTier changes only the tier column on a workspace's plan
// row, leaving any admin-configured per-workspace limit overrides
// (max_storage_bytes, max_users, max_bandwidth_bytes_monthly)
// untouched. Used by the Stripe webhook so a routine subscription
// event doesn't silently null out custom limits.
//
// When no row exists yet the row is created with NULL limits, which
// is the same behaviour as the admin "create plan" flow with no
// overrides — the per-tier defaults from TierDefaults apply.
func (r *PostgresRepository) UpdateTier(ctx context.Context, workspaceID uuid.UUID, tier string) (*Plan, error) {
	const q = `
INSERT INTO workspace_plans (workspace_id, tier)
VALUES ($1, $2)
ON CONFLICT (workspace_id) DO UPDATE SET
    tier = EXCLUDED.tier,
    updated_at = now()
RETURNING id, workspace_id, tier, max_storage_bytes, max_users,
          max_bandwidth_bytes_monthly, stripe_customer_id, created_at, updated_at`
	out := &Plan{}
	err := r.pool.QueryRow(ctx, q, workspaceID, tier).Scan(
		&out.ID, &out.WorkspaceID, &out.Tier,
		&out.MaxStorageBytes, &out.MaxUsers, &out.MaxBandwidthBytesMonthly,
		&out.StripeCustomerID,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update tier: %w", err)
	}
	return out, nil
}

// SetStripeCustomerID writes only the stripe_customer_id column on
// a workspace's plan row, leaving tier and any admin-configured
// limit overrides untouched. Used by the Stripe webhook to capture
// the customer ID created during a Checkout flow so the admin
// portal-session endpoint can later look it up.
//
// When no row exists yet the row is seeded with the free tier and
// NULL limits — the same shape the admin "create plan with no
// overrides" flow produces.
func (r *PostgresRepository) SetStripeCustomerID(ctx context.Context, workspaceID uuid.UUID, customerID string) error {
	if customerID == "" {
		return errors.New("billing: stripe customer id required")
	}
	const q = `
INSERT INTO workspace_plans (workspace_id, tier, stripe_customer_id)
VALUES ($1, $2, $3)
ON CONFLICT (workspace_id) DO UPDATE SET
    stripe_customer_id = EXCLUDED.stripe_customer_id,
    updated_at = now()`
	if _, err := r.pool.Exec(ctx, q, workspaceID, TierFree, customerID); err != nil {
		return fmt.Errorf("set stripe customer id: %w", err)
	}
	return nil
}

// RecordEvent appends a usage_events row.
func (r *PostgresRepository) RecordEvent(ctx context.Context, workspaceID uuid.UUID, eventType string, bytes int64) error {
	const q = `
INSERT INTO usage_events (workspace_id, event_type, bytes)
VALUES ($1, $2, $3)`
	if _, err := r.pool.Exec(ctx, q, workspaceID, eventType, bytes); err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// GetStorageUsed reads the live total from the files table rather
// than aggregating usage_events. Storage is the truth — events are
// audit trail. Soft-deleted rows are excluded.
func (r *PostgresRepository) GetStorageUsed(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	const q = `
SELECT COALESCE(SUM(size_bytes), 0)::BIGINT
FROM files
WHERE workspace_id = $1 AND deleted_at IS NULL`
	var total int64
	if err := r.pool.QueryRow(ctx, q, workspaceID).Scan(&total); err != nil {
		return 0, fmt.Errorf("get storage used: %w", err)
	}
	return total, nil
}

// GetBandwidthUsedThisMonth sums bandwidth events from the start of
// the current calendar month (UTC). The window is computed in
// Postgres so leap-second weirdness on the client is irrelevant.
func (r *PostgresRepository) GetBandwidthUsedThisMonth(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	const q = `
SELECT COALESCE(SUM(bytes), 0)::BIGINT
FROM usage_events
WHERE workspace_id = $1
  AND event_type = $2
  AND created_at >= date_trunc('month', now())`
	var total int64
	if err := r.pool.QueryRow(ctx, q, workspaceID, EventBandwidth).Scan(&total); err != nil {
		return 0, fmt.Errorf("get bandwidth used: %w", err)
	}
	return total, nil
}

// GetUserCount returns the active user count (non-deactivated rows)
// for a workspace.
func (r *PostgresRepository) GetUserCount(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	const q = `
SELECT COUNT(*)::INT
FROM users
WHERE workspace_id = $1 AND deactivated_at IS NULL`
	var n int
	if err := r.pool.QueryRow(ctx, q, workspaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("get user count: %w", err)
	}
	return n, nil
}
