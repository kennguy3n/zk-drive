package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository defines persistence operations for audit_log.
type Repository interface {
	Log(ctx context.Context, entry *Entry) error
	List(ctx context.Context, workspaceID uuid.UUID, action string, limit, offset int) ([]*Entry, error)
	// VerifyChain recomputes the per-workspace HMAC hash chain over
	// the live audit_log rows and confirms it terminates at the
	// separately-stored chain head. See PostgresRepository.VerifyChain.
	VerifyChain(ctx context.Context, workspaceID uuid.UUID) (*ChainVerification, error)
}

// PostgresRepository implements Repository against Postgres.
type PostgresRepository struct {
	pool   *pgxpool.Pool
	hasher *hasher
}

// NewPostgresRepository returns a PostgresRepository using the supplied
// pool and the audit-chain HMAC key (config.AuditHMACKey). The key is
// required: the hash chain (6.6) is not optional, so a nil/empty key is
// a configuration bug and construction fails fast rather than silently
// writing unauthenticated rows.
func NewPostgresRepository(pool *pgxpool.Pool, hmacKey []byte) (*PostgresRepository, error) {
	h, err := newHasher(hmacKey)
	if err != nil {
		return nil, err
	}
	return &PostgresRepository{pool: pool, hasher: h}, nil
}

const auditColumns = "id, workspace_id, actor_id, action, resource_type, resource_id, host(ip_address), user_agent, metadata, created_at, seq, prev_hash, entry_hash"

// Log inserts an audit_log row synchronously, extending the
// tamper-evident per-workspace hash chain (6.6) in the same
// transaction. Callers should route writes through Service, which
// wraps this with a background worker.
//
// Correctness under concurrency (multiple replicas each run their own
// audit worker): the chain head row is locked FOR UPDATE, so the
// read-prev / compute / insert / advance-head sequence is serialised
// per workspace at the database. The genesis head row is materialised
// with INSERT ... ON CONFLICT DO NOTHING before the lock so two racing
// first-writers cannot both claim seq 1.
func (r *PostgresRepository) Log(ctx context.Context, entry *Entry) error {
	if entry.ID == uuid.Nil {
		entry.ID = uuid.New()
	}
	// Stamp created_at in-process, truncated to microseconds so the
	// value we hash matches the TIMESTAMPTZ that round-trips from
	// Postgres on verification.
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	entry.CreatedAt = entry.CreatedAt.UTC().Truncate(time.Microsecond)

	var metadata any
	if len(entry.Metadata) > 0 {
		metadata = []byte(entry.Metadata)
	}
	var ip any
	if entry.IPAddress != nil && *entry.IPAddress != "" {
		ip = *entry.IPAddress
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("insert audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	genesis := r.hasher.genesis(entry.WorkspaceID)
	// Ensure a head row exists (seq 0, head = genesis) so the
	// subsequent SELECT ... FOR UPDATE always locks a concrete row.
	if _, err := tx.Exec(ctx,
		`INSERT INTO audit_log_chain_head (workspace_id, seq, head_hash)
		 VALUES ($1, 0, $2) ON CONFLICT (workspace_id) DO NOTHING`,
		entry.WorkspaceID, genesis,
	); err != nil {
		return fmt.Errorf("insert audit: ensure chain head: %w", err)
	}

	var prevSeq int64
	var prevHash []byte
	if err := tx.QueryRow(ctx,
		`SELECT seq, head_hash FROM audit_log_chain_head WHERE workspace_id = $1 FOR UPDATE`,
		entry.WorkspaceID,
	).Scan(&prevSeq, &prevHash); err != nil {
		return fmt.Errorf("insert audit: lock chain head: %w", err)
	}

	seq := prevSeq + 1
	entryHash, err := r.hasher.compute(entry, seq, prevHash)
	if err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}

	const insertQ = `
INSERT INTO audit_log (id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata, created_at, seq, prev_hash, entry_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7::inet, $8, $9, $10, $11, $12, $13)`
	if _, err := tx.Exec(ctx, insertQ,
		entry.ID, entry.WorkspaceID, entry.ActorID, entry.Action,
		entry.ResourceType, entry.ResourceID, ip, entry.UserAgent, metadata,
		entry.CreatedAt, seq, prevHash, entryHash,
	); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE audit_log_chain_head SET seq = $2, head_hash = $3, updated_at = now() WHERE workspace_id = $1`,
		entry.WorkspaceID, seq, entryHash,
	); err != nil {
		return fmt.Errorf("insert audit: advance chain head: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("insert audit: commit: %w", err)
	}

	entry.Seq = seq
	entry.PrevHash = prevHash
	entry.EntryHash = entryHash
	return nil
}

// ChainHead mirrors one audit_log_chain_head row: the latest sequence
// number and head hash for a workspace, plus the time it last advanced.
// An external verifier periodically snapshots these so a later
// wholesale rewrite of audit_log (even one that recomputes every per-
// row hash consistently) is still detectable — the retained head_hash
// will not match.
type ChainHead struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Seq         int64     `json:"seq"`
	HeadHash    []byte    `json:"head_hash"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ChainVerification reports the outcome of VerifyChain.
type ChainVerification struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	// Valid is true when every live row's HMAC recomputes, the rows
	// link contiguously, and the final row matches the stored head.
	Valid bool `json:"valid"`
	// RowsChecked is the number of live audit_log rows walked.
	RowsChecked int64 `json:"rows_checked"`
	// HeadSeq is the sequence number recorded in audit_log_chain_head
	// (0 when the workspace has no audit history yet).
	HeadSeq int64 `json:"head_seq"`
	// FirstInvalidSeq is the sequence number of the first row that
	// failed verification, or 0 when Valid is true.
	FirstInvalidSeq int64 `json:"first_invalid_seq,omitempty"`
	// Detail is a human-readable explanation when Valid is false.
	Detail string `json:"detail,omitempty"`
}

// ChainHead returns the stored chain head for a workspace. When the
// workspace has no audit history the returned head has Seq==0 and a nil
// HeadHash with a nil error, so callers can treat "never written" as a
// valid empty chain.
func (r *PostgresRepository) ChainHead(ctx context.Context, workspaceID uuid.UUID) (*ChainHead, error) {
	head := &ChainHead{WorkspaceID: workspaceID}
	err := r.pool.QueryRow(ctx,
		`SELECT seq, head_hash, updated_at FROM audit_log_chain_head WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&head.Seq, &head.HeadHash, &head.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return head, nil
	}
	if err != nil {
		return nil, fmt.Errorf("audit chain head: %w", err)
	}
	return head, nil
}

// VerifyChain walks a workspace's live audit_log rows in sequence order
// and confirms the HMAC hash chain (6.6) is intact:
//
//   - each row's entry_hash recomputes from its stored fields,
//   - each row's prev_hash equals the predecessor's entry_hash
//     (contiguity; no gaps, no inserted/removed rows), and
//   - the final row's entry_hash equals the separately-stored chain
//     head (and the head's seq matches).
//
// It tolerates cold-tier archival: the archiver deletes the oldest
// rows but leaves the chain head intact, so verification of a partially
// archived log starts at the OLDEST live row and trusts that row's
// stored prev_hash as the boundary (the archived JSONL retains the
// hashes needed to verify the deleted prefix offline). The boundary
// row's own entry_hash is still recomputed, so tampering with any live
// row — including the oldest — is detected; only the link to the
// already-archived predecessor is taken on trust.
//
// The walk streams rows under a snapshot read so memory stays O(1) in
// the number of audit rows.
func (r *PostgresRepository) VerifyChain(ctx context.Context, workspaceID uuid.UUID) (*ChainVerification, error) {
	head, err := r.ChainHead(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	res := &ChainVerification{WorkspaceID: workspaceID, HeadSeq: head.Seq}

	rows, err := r.pool.Query(ctx,
		`SELECT `+auditColumns+` FROM audit_log WHERE workspace_id = $1 AND seq IS NOT NULL ORDER BY seq ASC`,
		workspaceID,
	)
	if err != nil {
		return nil, fmt.Errorf("verify chain: query rows: %w", err)
	}
	defer rows.Close()

	var (
		prevSeq  int64
		prevHash []byte
		lastHash []byte
		lastSeq  int64
		first    = true
	)
	for rows.Next() {
		e, scanErr := scanRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("verify chain: scan: %w", scanErr)
		}
		res.RowsChecked++

		if !first {
			if e.Seq != prevSeq+1 {
				res.FirstInvalidSeq = e.Seq
				res.Detail = fmt.Sprintf("sequence gap: row seq %d follows %d", e.Seq, prevSeq)
				return res, nil
			}
			if !bytes.Equal(e.PrevHash, prevHash) {
				res.FirstInvalidSeq = e.Seq
				res.Detail = fmt.Sprintf("broken link at seq %d: prev_hash does not match predecessor entry_hash", e.Seq)
				return res, nil
			}
		}

		want, hErr := r.hasher.compute(e, e.Seq, e.PrevHash)
		if hErr != nil {
			return nil, fmt.Errorf("verify chain: recompute seq %d: %w", e.Seq, hErr)
		}
		if !bytes.Equal(want, e.EntryHash) {
			res.FirstInvalidSeq = e.Seq
			res.Detail = fmt.Sprintf("entry_hash mismatch at seq %d: row has been altered", e.Seq)
			return res, nil
		}

		prevSeq = e.Seq
		prevHash = e.EntryHash
		lastHash = e.EntryHash
		lastSeq = e.Seq
		first = false
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify chain: iterate: %w", err)
	}

	// The newest live row must match the separately-stored head. When
	// there are no live rows (fully archived or never written) but the
	// head records history, that is consistent only if archival
	// removed them; we cannot re-derive the head from an empty live
	// set, so we treat an empty live set as vacuously valid and let
	// the cold-tier verifier cover the archived rows.
	if res.RowsChecked == 0 {
		res.Valid = true
		return res, nil
	}
	if lastSeq != head.Seq {
		res.FirstInvalidSeq = lastSeq
		res.Detail = fmt.Sprintf("head seq %d does not match newest live row seq %d (rows appended or truncated out of band)", head.Seq, lastSeq)
		return res, nil
	}
	if !bytes.Equal(lastHash, head.HeadHash) {
		res.FirstInvalidSeq = lastSeq
		res.Detail = "head_hash does not match newest live row entry_hash"
		return res, nil
	}

	res.Valid = true
	return res, nil
}

// List returns paginated workspace audit entries, newest first. When
// action is non-empty the result is filtered by the action column.
func (r *PostgresRepository) List(ctx context.Context, workspaceID uuid.UUID, action string, limit, offset int) ([]*Entry, error) {
	limit, offset = normalizePaging(limit, offset)
	var rows pgx.Rows
	var err error
	if action == "" {
		q := "SELECT " + auditColumns + ` FROM audit_log
WHERE workspace_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3`
		rows, err = r.pool.Query(ctx, q, workspaceID, limit, offset)
	} else {
		q := "SELECT " + auditColumns + ` FROM audit_log
WHERE workspace_id = $1 AND action = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4`
		rows, err = r.pool.Query(ctx, q, workspaceID, action, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("list audit: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

func scanRows(rows pgx.Rows) ([]*Entry, error) {
	var out []*Entry
	for rows.Next() {
		// pgx.ErrNoRows is returned by QueryRow().Scan(), never by
		// iteration via rows.Next() + rows.Scan(); rows.Next()
		// returns false once exhausted, so a no-rows condition cannot
		// reach scanRow's Scan call.
		e, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanRow scans a single audit_log row (in auditColumns order) into an
// Entry. The hash-chain columns (seq, prev_hash, entry_hash) are
// nullable so rows written before migration 040 — which carry no chain
// data — scan cleanly with zero-value Seq and nil hashes.
func scanRow(rows pgx.Rows) (*Entry, error) {
	e := &Entry{}
	var (
		metadata  []byte
		ipAddress *string
		userAgent *string
		seq       *int64
		prevHash  []byte
		entryHash []byte
	)
	if err := rows.Scan(
		&e.ID, &e.WorkspaceID, &e.ActorID, &e.Action,
		&e.ResourceType, &e.ResourceID, &ipAddress, &userAgent, &metadata, &e.CreatedAt,
		&seq, &prevHash, &entryHash,
	); err != nil {
		return nil, err
	}
	if ipAddress != nil {
		e.IPAddress = ipAddress
	}
	if userAgent != nil {
		e.UserAgent = userAgent
	}
	if len(metadata) > 0 {
		e.Metadata = metadata
	}
	if seq != nil {
		e.Seq = *seq
	}
	e.PrevHash = prevHash
	e.EntryHash = entryHash
	return e, nil
}

func normalizePaging(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
