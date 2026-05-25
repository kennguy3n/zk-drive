package document

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository defines persistence operations for documents and their
// delta logs. Implementations must enforce per-tenant isolation via
// the standard RLS GUC plumbing; this interface accepts workspace_id
// explicitly for defence-in-depth filtering on top of RLS.
type Repository interface {
	Create(ctx context.Context, d *Document) error
	GetByID(ctx context.Context, workspaceID, documentID uuid.UUID) (*Document, error)

	// GetMetadata is the binary-free companion to GetByID. It returns
	// every column except y_state / y_state_vector (the potentially
	// MB-scale Yjs blobs), so callers that only need folder_id /
	// collab_mode / name (permission checks, activity logging, the
	// AppendDelta hot path) don't pay the Postgres I/O + Go heap
	// cost of streaming the binary state. The returned Document's
	// YState / YStateVector fields are nil — call GetByID or
	// GetSnapshotBundle when the Yjs bytes are actually needed.
	GetMetadata(ctx context.Context, workspaceID, documentID uuid.UUID) (*Document, error)

	UpdateName(ctx context.Context, workspaceID, documentID uuid.UUID, name string) (*Document, error)
	UpdateCollabMode(ctx context.Context, workspaceID, documentID uuid.UUID, collabMode string) (*Document, error)
	SoftDelete(ctx context.Context, workspaceID, documentID uuid.UUID) error
	ListByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Document, error)

	// ListByFolderSubtree returns every non-deleted document under the
	// given folder, including documents in descendant folders.
	// Mirrors the recursive CTE used by folder.SoftDeleteSubtree so the
	// two stay in lockstep: any document the cascade would soft-delete
	// shows up here, and only those documents. Callers snapshot the
	// slice BEFORE folder.SoftDeleteSubtree runs so they can emit one
	// activity / changefeed event per cascaded document AFTER the
	// folder delete commits. Uses documentListColumns (binary blobs
	// excluded) since callers only need metadata for the emit phase.
	ListByFolderSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Document, error)

	// AppendDelta inserts a single delta with a per-document
	// monotonic seq. The implementation assigns seq atomically via
	// `SELECT COALESCE(MAX(seq), 0) + 1 ... FOR UPDATE` inside the
	// same transaction as the INSERT, so concurrent writers serialise
	// rather than collide on the (document_id, seq) primary key. The
	// document's updated_at is bumped in the same transaction.
	AppendDelta(ctx context.Context, d *Delta) error

	// ListDeltas returns deltas with seq strictly greater than
	// `afterSeq`, ordered ascending, limited to `limit` rows. Callers
	// page by re-issuing with the highest seq from the previous
	// response as the new `afterSeq`. `afterSeq = 0` returns the
	// oldest available deltas.
	ListDeltas(ctx context.Context, workspaceID, documentID uuid.UUID, afterSeq int64, limit int) ([]*Delta, error)

	// CountDeltasAfter returns the number of deltas with seq strictly
	// greater than `afterSeq`. Used by the service to decide whether
	// the compaction threshold has been crossed.
	CountDeltasAfter(ctx context.Context, workspaceID, documentID uuid.UUID, afterSeq int64) (int64, error)

	// ReplaceSnapshot atomically updates the document's y_state,
	// y_state_vector, y_state_seq_floor (= upToSeq) and bumps
	// snapshot_version, then deletes every delta with seq <= upToSeq
	// for this document. Used by the compaction path. All four
	// changes happen in a single SERIALIZABLE transaction so an
	// observer either sees (snapshot vN, deltas seq > floorN) or
	// (snapshot vN+1, deltas seq > floorN+1) but never a torn
	// state. Returns the updated Document.
	//
	// expectedSnapshotVersion is the snapshot_version observed at the
	// start of the caller's read (typically via GetSnapshotBundle).
	// If the current row's snapshot_version no longer matches — i.e.
	// a concurrent Compact landed between the caller's read and its
	// write — the call fails with ErrSnapshotVersionConflict so the
	// caller can re-read + re-fold rather than write a fold computed
	// against stale state. Pass 0 to skip the optimistic-concurrency
	// check (e.g. initialisation paths that have no prior read).
	ReplaceSnapshot(ctx context.Context, workspaceID, documentID uuid.UUID, yState, yStateVector []byte, upToSeq int64, expectedSnapshotVersion int64) (*Document, error)

	// GetSnapshotBundle reads the document row AND its tail deltas
	// (seq > y_state_seq_floor) in a single REPEATABLE READ
	// transaction so a concurrent ReplaceSnapshot can't tear the
	// observed state. Without this, a Snapshot caller could read
	// (old y_state, old floor) and then a Compact could land between
	// the two reads, deleting the deltas that the Snapshot caller
	// was about to fetch — leaving the client with a stale snapshot
	// and a gap in its delta history.
	GetSnapshotBundle(ctx context.Context, workspaceID, documentID uuid.UUID, tailLimit int) (*Document, []*Delta, error)
}

// PostgresRepository implements Repository against Postgres via pgxpool.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

// NewPostgresRepository returns a PostgresRepository using the supplied pool.
func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const documentColumns = "id, workspace_id, folder_id, name, collab_mode, y_state, y_state_vector, y_state_seq_floor, snapshot_version, created_by, created_at, updated_at, deleted_at"

// documentListColumns is documentColumns minus the (potentially large)
// binary Yjs blobs. Used by list endpoints where the response struct
// hides those fields anyway — selecting them would burn Postgres I/O
// and Go heap for nothing. A scanned row's YState / YStateVector are
// left nil; consumers that need them must call GetByID / Snapshot.
const documentListColumns = "id, workspace_id, folder_id, name, collab_mode, y_state_seq_floor, snapshot_version, created_by, created_at, updated_at, deleted_at"

func scanDocument(row pgx.Row) (*Document, error) {
	d := &Document{}
	if err := row.Scan(
		&d.ID, &d.WorkspaceID, &d.FolderID, &d.Name, &d.CollabMode,
		&d.YState, &d.YStateVector, &d.YStateSeqFloor, &d.SnapshotVersion,
		&d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// scanDocumentListItem is the binary-free companion to scanDocument.
// YState / YStateVector are left as nil byte slices.
func scanDocumentListItem(row pgx.Row) (*Document, error) {
	d := &Document{}
	if err := row.Scan(
		&d.ID, &d.WorkspaceID, &d.FolderID, &d.Name, &d.CollabMode,
		&d.YStateSeqFloor, &d.SnapshotVersion,
		&d.CreatedBy, &d.CreatedAt, &d.UpdatedAt, &d.DeletedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// Create inserts a document row, populating the server-side
// timestamp / id columns on the supplied struct.
func (r *PostgresRepository) Create(ctx context.Context, d *Document) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.CollabMode == "" {
		d.CollabMode = CollabModeMarkdown
	}
	const q = `
INSERT INTO documents (id, workspace_id, folder_id, name, collab_mode, y_state, y_state_vector, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING y_state_seq_floor, snapshot_version, created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q,
		d.ID, d.WorkspaceID, d.FolderID, d.Name, d.CollabMode,
		d.YState, d.YStateVector, d.CreatedBy,
	).Scan(&d.YStateSeqFloor, &d.SnapshotVersion, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return fmt.Errorf("insert document: %w", err)
	}
	return nil
}

// GetByID fetches a non-deleted document.
func (r *PostgresRepository) GetByID(ctx context.Context, workspaceID, documentID uuid.UUID) (*Document, error) {
	q := "SELECT " + documentColumns + " FROM documents WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	return scanDocument(r.pool.QueryRow(ctx, q, workspaceID, documentID))
}

// GetMetadata fetches a non-deleted document but skips the
// (potentially large) Yjs binary columns. Used by the permission /
// capability-check paths that never need y_state.
func (r *PostgresRepository) GetMetadata(ctx context.Context, workspaceID, documentID uuid.UUID) (*Document, error) {
	q := "SELECT " + documentListColumns + " FROM documents WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	return scanDocumentListItem(r.pool.QueryRow(ctx, q, workspaceID, documentID))
}

// UpdateName changes the document's name and bumps updated_at.
func (r *PostgresRepository) UpdateName(ctx context.Context, workspaceID, documentID uuid.UUID, name string) (*Document, error) {
	q := `
UPDATE documents
   SET name = $3, updated_at = NOW()
 WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING ` + documentColumns
	return scanDocument(r.pool.QueryRow(ctx, q, workspaceID, documentID, name))
}

// UpdateCollabMode changes the document's collab_mode and bumps
// updated_at. The service layer validates against the folder's
// encryption mode before calling.
func (r *PostgresRepository) UpdateCollabMode(ctx context.Context, workspaceID, documentID uuid.UUID, collabMode string) (*Document, error) {
	q := `
UPDATE documents
   SET collab_mode = $3, updated_at = NOW()
 WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
RETURNING ` + documentColumns
	return scanDocument(r.pool.QueryRow(ctx, q, workspaceID, documentID, collabMode))
}

// SoftDelete marks the document deleted. Deltas are NOT trimmed —
// keeping them lets an admin restore the document later. A future
// retention job can hard-delete deltas for documents deleted more
// than N days ago.
func (r *PostgresRepository) SoftDelete(ctx context.Context, workspaceID, documentID uuid.UUID) error {
	const q = `
UPDATE documents
   SET deleted_at = NOW(), updated_at = NOW()
 WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, documentID)
	if err != nil {
		return fmt.Errorf("soft-delete document: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListByFolder returns all non-deleted documents in a folder
// ordered by most-recently-updated first (matches the UI's
// document-list panel default sort). Capped at MaxDocumentsPerFolder
// so a pathological folder cannot slow-list the whole table — the
// UI paginates well below this in normal operation. Uses
// documentListColumns to skip the (potentially MB-scale) Yjs binary
// blobs since the API response struct hides them anyway; callers
// who need YState must round-trip via GetByID / Snapshot.
func (r *PostgresRepository) ListByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Document, error) {
	q := `
SELECT ` + documentListColumns + `
  FROM documents
 WHERE workspace_id = $1 AND folder_id = $2 AND deleted_at IS NULL
 ORDER BY updated_at DESC
 LIMIT $3`
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID, MaxDocumentsPerFolder)
	if err != nil {
		return nil, fmt.Errorf("list documents by folder: %w", err)
	}
	defer rows.Close()

	var out []*Document
	for rows.Next() {
		d, err := scanDocumentListItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListByFolderSubtree walks the folder subtree rooted at folderID
// and returns every non-deleted document underneath. Mirrors the
// recursive CTE used by folder.SoftDeleteSubtree so the two stay in
// lockstep — any document the cascade would soft-delete shows up
// here. Used by the DeleteFolder / BulkDelete handlers to snapshot
// documents BEFORE the folder soft-delete cascades to them, so the
// handlers can emit one ActionDocumentDelete activity + changefeed
// event per cascaded document AFTER the folder delete commits.
func (r *PostgresRepository) ListByFolderSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*Document, error) {
	const q = `
WITH RECURSIVE subtree AS (
    SELECT id FROM folders
     WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT f.id FROM folders f
      JOIN subtree s ON f.parent_folder_id = s.id
     WHERE f.workspace_id = $1 AND f.deleted_at IS NULL
)
SELECT ` + documentListColumns + `
  FROM documents
 WHERE workspace_id = $1 AND folder_id IN (SELECT id FROM subtree)
   AND deleted_at IS NULL
 ORDER BY folder_id, updated_at DESC`
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list documents in folder subtree: %w", err)
	}
	defer rows.Close()

	var out []*Document
	for rows.Next() {
		d, err := scanDocumentListItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AppendDelta inserts a delta atomically with a per-document seq.
// The transaction uses READ COMMITTED — NOT REPEATABLE READ. Under
// REPEATABLE READ, concurrent AppendDelta callers would observe
// 'could not serialize access due to concurrent update' errors
// when the second tx unblocks on FOR UPDATE and discovers the
// documents row was modified by the now-committed first tx (PG
// raises 40001 in this case per the docs). Under READ COMMITTED
// each statement re-evaluates against the latest committed
// snapshot, so the second tx's COALESCE(MAX(seq), 0) + 1 query
// correctly observes the first tx's inserted delta and produces
// the next sequential seq. The FOR UPDATE on the documents row
// remains the serialisation point for concurrent writers; the
// isolation level downgrade only changes what happens AFTER the
// lock is acquired.
func (r *PostgresRepository) AppendDelta(ctx context.Context, d *Delta) error {
	if len(d.Payload) == 0 {
		return ErrEmptyPayload
	}
	if len(d.Payload) > MaxDeltaPayloadBytes {
		return ErrPayloadTooLarge
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the document row so concurrent AppendDelta callers
	// serialise. A SELECT FOR UPDATE on documents would also work,
	// but advisory lock on the documents row keeps the lock scope
	// narrow.
	var exists bool
	if err := tx.QueryRow(ctx, `
SELECT TRUE
  FROM documents
 WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
 FOR UPDATE`, d.WorkspaceID, d.DocumentID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock document: %w", err)
	}

	// Compute next seq for this document.
	var nextSeq int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(MAX(seq), 0) + 1
  FROM document_deltas
 WHERE document_id = $1`, d.DocumentID).Scan(&nextSeq); err != nil {
		return fmt.Errorf("compute next seq: %w", err)
	}
	d.Seq = nextSeq

	if err := tx.QueryRow(ctx, `
INSERT INTO document_deltas (document_id, seq, payload, author_user_id, workspace_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at`, d.DocumentID, d.Seq, d.Payload, d.AuthorUserID, d.WorkspaceID).
		Scan(&d.CreatedAt); err != nil {
		return fmt.Errorf("insert delta: %w", err)
	}

	if _, err := tx.Exec(ctx, `
UPDATE documents
   SET updated_at = NOW()
 WHERE workspace_id = $1 AND id = $2`, d.WorkspaceID, d.DocumentID); err != nil {
		return fmt.Errorf("bump document updated_at: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delta: %w", err)
	}
	return nil
}

// ListDeltas returns deltas above the supplied cursor. The seq
// column is strictly monotonic per document and a single row's seq
// never changes, so paging is stable under concurrent appends.
func (r *PostgresRepository) ListDeltas(ctx context.Context, workspaceID, documentID uuid.UUID, afterSeq int64, limit int) ([]*Delta, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxDeltaListLimit {
		limit = MaxDeltaListLimit
	}
	const q = `
SELECT document_id, seq, payload, author_user_id, created_at, workspace_id
  FROM document_deltas
 WHERE workspace_id = $1 AND document_id = $2 AND seq > $3
 ORDER BY seq ASC
 LIMIT $4`
	rows, err := r.pool.Query(ctx, q, workspaceID, documentID, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("list deltas: %w", err)
	}
	defer rows.Close()

	var out []*Delta
	for rows.Next() {
		d := &Delta{}
		if err := rows.Scan(&d.DocumentID, &d.Seq, &d.Payload, &d.AuthorUserID, &d.CreatedAt, &d.WorkspaceID); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountDeltasAfter returns the number of deltas above the cursor.
// Used by the compaction path to decide whether the threshold has
// been crossed without paging the full list.
func (r *PostgresRepository) CountDeltasAfter(ctx context.Context, workspaceID, documentID uuid.UUID, afterSeq int64) (int64, error) {
	var count int64
	if err := r.pool.QueryRow(ctx, `
SELECT COUNT(*)
  FROM document_deltas
 WHERE workspace_id = $1 AND document_id = $2 AND seq > $3`,
		workspaceID, documentID, afterSeq).Scan(&count); err != nil {
		return 0, fmt.Errorf("count deltas: %w", err)
	}
	return count, nil
}

// ReplaceSnapshot atomically updates the snapshot + trims folded
// deltas. The transaction is SERIALIZABLE because we read deltas
// up to a seq, compute their effect into yState (done by the
// caller), and then both rewrite the snapshot AND delete the
// folded deltas — a concurrent AppendDelta would otherwise be able
// to insert a delta with seq <= upToSeq between our compute and
// our DELETE.
func (r *PostgresRepository) ReplaceSnapshot(ctx context.Context, workspaceID, documentID uuid.UUID, yState, yStateVector []byte, upToSeq int64, expectedSnapshotVersion int64) (*Document, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("begin compaction tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Optimistic concurrency: only update if snapshot_version is what
	// the caller observed. expectedSnapshotVersion=0 opts out (used by
	// init paths that never read a prior snapshot). When a concurrent
	// Compact has bumped snapshot_version, the UPDATE matches zero
	// rows, RETURNING reports ErrNoRows, and we map it to
	// ErrSnapshotVersionConflict so the caller can re-read + retry.
	q := `
UPDATE documents
   SET y_state = $3,
       y_state_vector = $4,
       y_state_seq_floor = GREATEST(y_state_seq_floor, $5),
       snapshot_version = snapshot_version + 1,
       updated_at = NOW()
 WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
   AND ($6 = 0 OR snapshot_version = $6)
RETURNING ` + documentColumns
	d, err := scanDocument(tx.QueryRow(ctx, q, workspaceID, documentID, yState, yStateVector, upToSeq, expectedSnapshotVersion))
	if err != nil {
		if isPgSerializationFailure(err) {
			// SERIALIZABLE conflicted with a concurrent writer (most
			// likely AppendDelta bumping updated_at on the documents
			// row). Surface this as ErrSnapshotVersionConflict so
			// the caller's retry path is uniform — they re-read the
			// (now updated) tail and re-fold against fresh state
			// instead of needing to recognise PG's raw 40001.
			return nil, ErrSnapshotVersionConflict
		}
		if errors.Is(err, ErrNotFound) && expectedSnapshotVersion != 0 {
			// Disambiguate "row gone" from "version mismatch" — if the
			// caller supplied a non-zero expected version and the
			// row exists but with a different version, surface
			// ErrSnapshotVersionConflict. Quick existence probe.
			var exists bool
			if probeErr := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM documents WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL)`,
				workspaceID, documentID,
			).Scan(&exists); probeErr == nil && exists {
				return nil, ErrSnapshotVersionConflict
			}
		}
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
DELETE FROM document_deltas
 WHERE workspace_id = $1 AND document_id = $2 AND seq <= $3`,
		workspaceID, documentID, upToSeq); err != nil {
		if isPgSerializationFailure(err) {
			return nil, ErrSnapshotVersionConflict
		}
		return nil, fmt.Errorf("trim folded deltas: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		if isPgSerializationFailure(err) {
			return nil, ErrSnapshotVersionConflict
		}
		return nil, fmt.Errorf("commit compaction: %w", err)
	}
	return d, nil
}

// pgSerializationFailure is the SQLSTATE class for
// 'could not serialize access due to concurrent update / read /
// dependency'. Surfaced by SERIALIZABLE / REPEATABLE READ
// transactions when a concurrent committer makes the snapshot
// inconsistent with the lock graph. Compared via the structured
// pgconn.PgError type (errors.As) rather than a substring match
// so we don't false-positive on error text that happens to contain
// the literal '40001' (numeric values in upstream error messages,
// unrelated SQLSTATE classes that share the suffix, etc.).
const pgSerializationFailure = "40001"

func isPgSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgSerializationFailure
	}
	return false
}

// GetSnapshotBundle reads the document + tail deltas atomically.
// REPEATABLE READ is sufficient: ReplaceSnapshot runs at
// SERIALIZABLE and touches both the documents row AND the
// document_deltas rows, so a concurrent compaction either commits
// fully before our tx snapshot is taken (we see the new floor +
// trimmed tail) or after our tx commits (we see the old floor +
// the deltas it will later trim). Either way the (snapshot, tail)
// pair we return is internally consistent.
func (r *PostgresRepository) GetSnapshotBundle(ctx context.Context, workspaceID, documentID uuid.UUID, tailLimit int) (*Document, []*Delta, error) {
	if tailLimit <= 0 {
		tailLimit = MaxDeltaPageLimit
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, fmt.Errorf("begin snapshot tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	docQ := "SELECT " + documentColumns + " FROM documents WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	d, err := scanDocument(tx.QueryRow(ctx, docQ, workspaceID, documentID))
	if err != nil {
		return nil, nil, err
	}

	rows, err := tx.Query(ctx, `
SELECT document_id, seq, payload, author_user_id, created_at, workspace_id
  FROM document_deltas
 WHERE workspace_id = $1 AND document_id = $2 AND seq > $3
 ORDER BY seq ASC
 LIMIT $4`, workspaceID, documentID, d.YStateSeqFloor, tailLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("list snapshot tail: %w", err)
	}
	defer rows.Close()

	var tail []*Delta
	for rows.Next() {
		delta := &Delta{}
		if err := rows.Scan(&delta.DocumentID, &delta.Seq, &delta.Payload, &delta.AuthorUserID, &delta.CreatedAt, &delta.WorkspaceID); err != nil {
			return nil, nil, err
		}
		tail = append(tail, delta)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, fmt.Errorf("commit snapshot tx: %w", err)
	}
	return d, tail, nil
}
