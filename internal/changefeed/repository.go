package changefeed

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	// BatchRecord writes len(muts) rows in a single multi-row INSERT
	// and populates Sequence + OccurredAt on each. Used by bulk
	// handlers (move/copy/delete of N items) to amortise the
	// per-row round-trip cost across an entire bulk operation while
	// preserving the durability guarantee that the catch-up cursor
	// advances before the HTTP response returns. Order is preserved:
	// muts[i].Sequence < muts[j].Sequence for i < j.
	BatchRecord(ctx context.Context, muts []Mutation) error
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

// BatchRecord persists len(muts) rows in a single multi-row INSERT.
// All rows land on adjacent sequence values because Postgres emits
// them in order from the BIGSERIAL sequence object during a single
// statement; that ordering is what lets sync clients page through
// the batch without missing a row.
//
// We deliberately do NOT wrap in a transaction: change_log INSERTs
// only INSERT (no follow-up writes), and the multi-row statement is
// already atomic from the perspective of any other reader. Adding
// BEGIN/COMMIT would just buy an extra round-trip.
func (r *PostgresRepository) BatchRecord(ctx context.Context, muts []Mutation) error {
	if len(muts) == 0 {
		return nil
	}
	// pgx's $N placeholders are positional, so build the VALUES
	// clause dynamically: ($1,$2,…,$8), ($9,…,$16), … for 8 cols.
	const cols = 8
	args := make([]any, 0, len(muts)*cols)
	var sb strings.Builder
	sb.Grow(64 + len(muts)*40)
	sb.WriteString("INSERT INTO change_log (workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata) VALUES ")
	for i, m := range muts {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('(')
		for j := 0; j < cols; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, "$%d", i*cols+j+1)
		}
		sb.WriteByte(')')
		var metadata any
		if len(m.Metadata) > 0 {
			metadata = []byte(m.Metadata)
		}
		args = append(args,
			m.WorkspaceID, m.ActorID, m.Kind, m.Op,
			m.ResourceID, m.ParentID, m.Name, metadata,
		)
	}
	sb.WriteString(" RETURNING sequence, occurred_at")
	rows, err := r.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return fmt.Errorf("batch insert change_log: %w", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		if i >= len(muts) {
			return errors.New("changefeed: batch insert returned more rows than inputs")
		}
		if err := rows.Scan(&muts[i].Sequence, &muts[i].OccurredAt); err != nil {
			return fmt.Errorf("scan batch change_log row: %w", err)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate batch change_log rows: %w", err)
	}
	if i != len(muts) {
		return fmt.Errorf("changefeed: batch insert returned %d rows, expected %d", i, len(muts))
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
