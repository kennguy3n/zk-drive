package changefeed

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository abstracts change_log persistence so the service can be
// unit-tested without a real Postgres instance.
type Repository interface {
	// Record writes a Mutation to change_log and populates m.Sequence
	// + m.OccurredAt from the inserted row. The caller passes a
	// Mutation with the user-supplied fields set; Sequence and
	// OccurredAt are returned from the database (BIGSERIAL +
	// DEFAULT now()).
	Record(ctx context.Context, m *Mutation) error
	// Since returns up to `limit` rows for the given workspace where
	// sequence > cursor, ordered by sequence ascending. Returns the
	// rows plus a boolean indicating whether more rows may exist
	// after the last one returned (i.e. whether limit was hit).
	Since(ctx context.Context, workspaceID uuid.UUID, cursor int64, limit int) ([]Mutation, bool, error)
	// Latest returns the highest sequence value currently stored for
	// the given workspace, or 0 if there are no rows yet. Sync
	// clients call this on initial connect to learn the "now" cursor
	// without having to scan the entire history.
	Latest(ctx context.Context, workspaceID uuid.UUID) (int64, error)
}

// PostgresRepository persists Mutations to the change_log table.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository constructs a PostgresRepository over pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const changeColumns = "sequence, workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata, occurred_at"

// Record inserts one Mutation. The (sequence, occurred_at) tuple is
// assigned by Postgres and read back via RETURNING so callers can
// immediately broadcast the durable row.
func (r *PostgresRepository) Record(ctx context.Context, m *Mutation) error {
	if m == nil {
		return errors.New("changefeed: nil mutation")
	}
	const q = `
INSERT INTO change_log (workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING sequence, occurred_at`
	var metadata any
	if len(m.Metadata) > 0 {
		metadata = []byte(m.Metadata)
	}
	if err := r.pool.QueryRow(ctx, q,
		m.WorkspaceID, m.ActorID, m.Kind, m.Op, m.ResourceID, m.ParentID, m.Name, metadata,
	).Scan(&m.Sequence, &m.OccurredAt); err != nil {
		return fmt.Errorf("insert change_log: %w", err)
	}
	return nil
}

// Since returns mutations with sequence > cursor, ordered by sequence
// ascending. `limit` is clamped to a safe range by the service before
// reaching here; this layer trusts its inputs.
func (r *PostgresRepository) Since(ctx context.Context, workspaceID uuid.UUID, cursor int64, limit int) ([]Mutation, bool, error) {
	// limit+1 is the standard "do we have a next page?" trick — pull
	// one more than asked, and if we got it, drop the extra row and
	// set hasMore=true. Avoids a second COUNT(*) query.
	q := "SELECT " + changeColumns + ` FROM change_log
WHERE workspace_id = $1 AND sequence > $2
ORDER BY sequence ASC
LIMIT $3`
	rows, err := r.pool.Query(ctx, q, workspaceID, cursor, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("query change_log: %w", err)
	}
	defer rows.Close()
	out := make([]Mutation, 0, limit)
	for rows.Next() {
		var m Mutation
		var actor *uuid.UUID
		var parent *uuid.UUID
		var name *string
		var metadataBytes []byte
		if err := rows.Scan(
			&m.Sequence, &m.WorkspaceID, &actor, &m.Kind, &m.Op,
			&m.ResourceID, &parent, &name, &metadataBytes, &m.OccurredAt,
		); err != nil {
			return nil, false, fmt.Errorf("scan change_log: %w", err)
		}
		m.ActorID = actor
		m.ParentID = parent
		if name != nil {
			m.Name = *name
		}
		if len(metadataBytes) > 0 {
			m.Metadata = metadataBytes
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("scan change_log rows: %w", err)
	}
	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	return out, hasMore, nil
}

// Latest returns the highest sequence value for the workspace, or 0
// when no rows exist. pgx.ErrNoRows is intentionally not surfaced;
// "empty workspace" is a normal state.
func (r *PostgresRepository) Latest(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	const q = `SELECT COALESCE(MAX(sequence), 0) FROM change_log WHERE workspace_id = $1`
	var seq int64
	if err := r.pool.QueryRow(ctx, q, workspaceID).Scan(&seq); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("max(change_log.sequence): %w", err)
	}
	return seq, nil
}
