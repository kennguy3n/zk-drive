package feature

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository persists per-workspace feature overrides.
type Repository interface {
	// GetOverrides returns the explicit per-workspace overrides for a
	// workspace as a feature-key → enabled map. Workspaces with no
	// overrides return an empty (non-nil) map. Only rows for known
	// features are returned; a key that is no longer recognised (e.g. a
	// feature removed in a later release) is ignored so stale rows can't
	// surface in the API.
	GetOverrides(ctx context.Context, workspaceID uuid.UUID) (map[string]bool, error)
	// SetOverride upserts a single feature override for a workspace.
	SetOverride(ctx context.Context, workspaceID uuid.UUID, feature string, enabled bool, updatedBy *uuid.UUID) error
	// DeleteOverride removes a feature override, reverting the feature to
	// its tier default. A no-op (no error) when no override exists.
	DeleteOverride(ctx context.Context, workspaceID uuid.UUID, feature string) error
}

// PostgresRepository implements Repository against the workspace_features
// table.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository backed by pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) GetOverrides(ctx context.Context, workspaceID uuid.UUID) (map[string]bool, error) {
	const q = `SELECT feature, enabled FROM workspace_features WHERE workspace_id = $1`
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("feature: query overrides: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var (
			key     string
			enabled bool
		)
		if err := rows.Scan(&key, &enabled); err != nil {
			return nil, fmt.Errorf("feature: scan override: %w", err)
		}
		// Ignore rows for features the running build no longer knows
		// about so a stale override can't leak into the API surface.
		if !IsKnownFeature(key) {
			continue
		}
		out[key] = enabled
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feature: iterate overrides: %w", err)
	}
	return out, nil
}

func (r *PostgresRepository) SetOverride(ctx context.Context, workspaceID uuid.UUID, feature string, enabled bool, updatedBy *uuid.UUID) error {
	const q = `
		INSERT INTO workspace_features (workspace_id, feature, enabled, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (workspace_id, feature)
		DO UPDATE SET enabled = EXCLUDED.enabled,
		              updated_by = EXCLUDED.updated_by,
		              updated_at = now()`
	if _, err := r.pool.Exec(ctx, q, workspaceID, feature, enabled, updatedBy); err != nil {
		return fmt.Errorf("feature: upsert override: %w", err)
	}
	return nil
}

func (r *PostgresRepository) DeleteOverride(ctx context.Context, workspaceID uuid.UUID, feature string) error {
	const q = `DELETE FROM workspace_features WHERE workspace_id = $1 AND feature = $2`
	if _, err := r.pool.Exec(ctx, q, workspaceID, feature); err != nil {
		return fmt.Errorf("feature: delete override: %w", err)
	}
	return nil
}

// compile-time assertion that PostgresRepository satisfies Repository.
var _ Repository = (*PostgresRepository)(nil)
