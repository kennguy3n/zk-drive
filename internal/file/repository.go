package file

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/zk-drive/internal/database"
)

// ErrNotFound is returned when the requested file (or version) does not
// exist within the supplied workspace.
var ErrNotFound = errors.New("file not found")

// ErrVersionConflict is returned by insertVersionTx when a row with
// the requested version UUID already exists but belongs to a
// different file or carries a different object_key. A legitimate
// retry of ConfirmUpload always re-submits the (versionID, fileID,
// objectKey) triple it was given by UploadURL, so a mismatch
// signals either a UUID forge attempt or a programming error.
var ErrVersionConflict = errors.New("file version conflicts with existing row")

// ErrTagAlreadyExists is returned when AddTag would violate the
// (file_id, tag) unique constraint. Distinct from ErrNotFound so the
// HTTP layer can map it to 409 Conflict.
var ErrTagAlreadyExists = errors.New("tag already exists on file")

// Repository defines persistence operations for files and file versions.
type Repository interface {
	CreateFile(ctx context.Context, f *File) error
	GetFileByID(ctx context.Context, workspaceID, fileID uuid.UUID) (*File, error)
	UpdateFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string, folderID uuid.UUID) error
	RenameFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string) error
	DeleteFile(ctx context.Context, workspaceID, fileID uuid.UUID) error
	MoveFile(ctx context.Context, workspaceID, fileID, folderID uuid.UUID) error
	UpdateFileSize(ctx context.Context, workspaceID, fileID uuid.UUID, sizeBytes int64) error
	ListFilesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error)
	// ListFilesInFolderSubtree returns every non-deleted file whose
	// folder_id is folderID OR any descendant of folderID. Used to
	// snapshot per-file metadata BEFORE a recursive folder soft-delete
	// so the cascade can emit a file.deleted webhook per affected file
	// (the corresponding folder.SoftDeleteSubtree cascades the files'
	// deleted_at column inside one transaction; the webhook cascade
	// must mirror that without leaving subscribers in the dark). Walks
	// the same folder-parent hierarchy via a recursive CTE so a deep
	// nested tree still returns the full set in one round trip.
	ListFilesInFolderSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error)

	// SetPendingUploadObjectKey records the presigned-PUT object_key
	// on the file row so the orphan-object GC reconciler can later
	// reclaim the S3 object if ConfirmUpload never completes (or is
	// rejected for quota / suspended-tenant / etc.). Called from the
	// UploadURL handler immediately after CreateFile + key derivation.
	SetPendingUploadObjectKey(ctx context.Context, workspaceID, fileID uuid.UUID, key string) error
	// ListPendingUploadOrphans returns file rows whose presigned upload
	// was minted before olderThan and which still have no confirmed
	// current_version_id. Drives the GC reconciler's scan loop.
	ListPendingUploadOrphans(ctx context.Context, workspaceID uuid.UUID, olderThan time.Time, limit int) ([]*PendingOrphan, error)
	// DeletePendingOrphan removes a file row that was previously
	// returned by ListPendingUploadOrphans. The transaction is
	// guarded by the pending_upload_object_key + current_version_id
	// IS NULL predicates so a concurrent ConfirmUpload racing the GC
	// pass cannot have its row deleted out from under it.
	DeletePendingOrphan(ctx context.Context, workspaceID, fileID uuid.UUID) error

	CreateFileVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error
	CreateVersionAndSetCurrent(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error
	// ConfirmVersion inserts a new version row, advances the file's
	// current_version_id, and updates files.size_bytes — all in one
	// transaction. The `fresh` return value is true when this call
	// created the version row, false when an existing row with the
	// same v.ID was found and the call was treated as an idempotent
	// retry. Callers that need to gate side effects (audit logs,
	// billing usage events, post-upload job dispatch) on the first
	// confirm vs. a network-retry MUST inspect `fresh`.
	ConfirmVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) (fresh bool, err error)
	ListVersions(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*FileVersion, error)
	GetVersionByID(ctx context.Context, workspaceID, versionID uuid.UUID) (*FileVersion, error)
	SetCurrentVersion(ctx context.Context, workspaceID, fileID, versionID uuid.UUID) error

	AddTag(ctx context.Context, workspaceID, fileID, createdBy uuid.UUID, tag string) (*Tag, error)
	RemoveTag(ctx context.Context, workspaceID, fileID uuid.UUID, tag string) error
	ListTagsByFile(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*Tag, error)
	ListTagsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]*Tag, error)
}

// PostgresRepository implements Repository against Postgres.
//
// pool is a database.Querier so the repository can be wired against a
// read/write splitter: SELECT-family reads (file listings, version
// lookups, tag queries) fan out to a Postgres read replica while writes
// and multi-statement transactions stay on the primary. A plain
// *pgxpool.Pool also satisfies database.Querier, so single-pool
// deployments are unaffected.
type PostgresRepository struct {
	pool database.Querier
}

// NewPostgresRepository returns a PostgresRepository using the supplied
// querier. Pass a *pgxpool.Pool for single-pool deployments or a
// *database.ReadWriteSplitter to fan reads out to a replica.
func NewPostgresRepository(db database.Querier) *PostgresRepository {
	return &PostgresRepository{pool: db}
}

const fileColumns = "id, workspace_id, folder_id, name, current_version_id, size_bytes, mime_type, created_by, created_at, updated_at, deleted_at"

func scanFile(row pgx.Row) (*File, error) {
	f := &File{}
	if err := row.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.CurrentVersionID, &f.SizeBytes, &f.MimeType, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// PendingOrphan is the projection returned by ListPendingUploadOrphans.
// It carries only the columns the GC reconciler needs to delete both
// the S3 object and the file row: the file id (for the DELETE), the
// recorded object_key (for the storage DeleteObject), and created_at
// (for metric labels / structured-log fields documenting the age of
// the orphan at reclaim time).
type PendingOrphan struct {
	FileID    uuid.UUID
	ObjectKey string
	CreatedAt time.Time
}

// CreateFile inserts a new file metadata row.
func (r *PostgresRepository) CreateFile(ctx context.Context, f *File) error {
	if f.ID == uuid.Nil {
		f.ID = uuid.New()
	}
	if f.MimeType == "" {
		f.MimeType = "application/octet-stream"
	}
	const q = `
INSERT INTO files (id, workspace_id, folder_id, name, size_bytes, mime_type, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING created_at, updated_at`
	if err := r.pool.QueryRow(ctx, q, f.ID, f.WorkspaceID, f.FolderID, f.Name, f.SizeBytes, f.MimeType, f.CreatedBy).
		Scan(&f.CreatedAt, &f.UpdatedAt); err != nil {
		return fmt.Errorf("insert file: %w", err)
	}
	return nil
}

// SetPendingUploadObjectKey stamps the presigned-PUT object key on
// the file row. Called from api/drive/upload.go:UploadURL right
// after CreateFile + NewObjectKey. The ConfirmVersion path clears
// the column transactionally with the version row insert; the
// orphan GC reconciler scans rows where the column is still set
// past the configured cooldown.
func (r *PostgresRepository) SetPendingUploadObjectKey(ctx context.Context, workspaceID, fileID uuid.UUID, key string) error {
	const q = `
UPDATE files SET pending_upload_object_key = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, key)
	if err != nil {
		return fmt.Errorf("set pending upload object key: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPendingUploadOrphans returns file rows with a recorded pending
// object key that have not been confirmed by olderThan. The partial
// index idx_files_pending_orphan is keyed on (workspace_id,
// created_at) with the same WHERE predicates as the query, so Postgres
// can satisfy the scan via the index without a sequential filter pass
// over the full files table. Selected columns (id, object_key,
// created_at) are not all in the index — strictly this is an index
// scan with heap fetches, not an index-only scan — but the partial
// predicate keeps the index tiny and the heap fetches scale with the
// orphan count rather than the workspace's full file population.
func (r *PostgresRepository) ListPendingUploadOrphans(ctx context.Context, workspaceID uuid.UUID, olderThan time.Time, limit int) ([]*PendingOrphan, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
SELECT id, pending_upload_object_key, created_at
FROM files
WHERE workspace_id = $1
  AND pending_upload_object_key IS NOT NULL
  AND current_version_id IS NULL
  AND deleted_at IS NULL
  AND created_at < $2
ORDER BY created_at ASC
LIMIT $3`
	rows, err := r.pool.Query(ctx, q, workspaceID, olderThan, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending orphans: %w", err)
	}
	defer rows.Close()
	var out []*PendingOrphan
	for rows.Next() {
		o := &PendingOrphan{}
		if err := rows.Scan(&o.FileID, &o.ObjectKey, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeletePendingOrphan removes an orphan file row. The WHERE clause
// re-checks the orphan predicates so a concurrent ConfirmUpload that
// races the GC scan-then-delete window cannot have its newly-
// confirmed row deleted by mistake — if ConfirmUpload landed between
// the list and this delete, current_version_id is now non-NULL and
// the DELETE matches zero rows.
func (r *PostgresRepository) DeletePendingOrphan(ctx context.Context, workspaceID, fileID uuid.UUID) error {
	const q = `
DELETE FROM files
WHERE workspace_id = $1
  AND id = $2
  AND pending_upload_object_key IS NOT NULL
  AND current_version_id IS NULL
  AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID)
	if err != nil {
		return fmt.Errorf("delete pending orphan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetFileByID returns a non-deleted file within a workspace.
func (r *PostgresRepository) GetFileByID(ctx context.Context, workspaceID, fileID uuid.UUID) (*File, error) {
	q := "SELECT " + fileColumns + " FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL"
	return scanFile(r.pool.QueryRow(ctx, q, workspaceID, fileID))
}

// UpdateFile renames a file and (optionally) moves it to a new folder in a
// single statement.
func (r *PostgresRepository) UpdateFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string, folderID uuid.UUID) error {
	const q = `
UPDATE files SET name = $3, folder_id = $4, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, name, folderID)
	if err != nil {
		return fmt.Errorf("update file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RenameFile updates only the name column so a concurrent MoveFile cannot
// be clobbered by a stale folder_id write.
func (r *PostgresRepository) RenameFile(ctx context.Context, workspaceID, fileID uuid.UUID, name string) error {
	const q = `
UPDATE files SET name = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, name)
	if err != nil {
		return fmt.Errorf("rename file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteFile soft-deletes a file by setting deleted_at.
func (r *PostgresRepository) DeleteFile(ctx context.Context, workspaceID, fileID uuid.UUID) error {
	const q = `
UPDATE files SET deleted_at = now(), updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID)
	if err != nil {
		return fmt.Errorf("delete file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MoveFile updates the folder_id of a file.
func (r *PostgresRepository) MoveFile(ctx context.Context, workspaceID, fileID, folderID uuid.UUID) error {
	const q = `
UPDATE files SET folder_id = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, folderID)
	if err != nil {
		return fmt.Errorf("move file: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateFileSize records the byte-size of a file's current version on the
// file row so listings can show size without joining file_versions.
func (r *PostgresRepository) UpdateFileSize(ctx context.Context, workspaceID, fileID uuid.UUID, sizeBytes int64) error {
	const q = `
UPDATE files SET size_bytes = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, sizeBytes)
	if err != nil {
		return fmt.Errorf("update file size: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListFilesByFolder returns non-deleted files inside a folder.
func (r *PostgresRepository) ListFilesByFolder(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error) {
	q := "SELECT " + fileColumns + " FROM files WHERE workspace_id = $1 AND folder_id = $2 AND deleted_at IS NULL ORDER BY name ASC"
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f := &File{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.CurrentVersionID, &f.SizeBytes, &f.MimeType, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ListFilesInFolderSubtree walks the folder tree rooted at folderID
// and returns every non-deleted file underneath. Mirrors the
// recursive CTE shape used by folder.SoftDeleteSubtree so the two
// stay in lockstep: any file the cascade would soft-delete shows up
// here, and only those files. Callers should snapshot the slice
// BEFORE issuing folders.Delete — once the cascade fires the
// deleted_at IS NULL filter would hide the affected rows.
func (r *PostgresRepository) ListFilesInFolderSubtree(ctx context.Context, workspaceID, folderID uuid.UUID) ([]*File, error) {
	const q = `
WITH RECURSIVE subtree AS (
    SELECT id FROM folders WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL
    UNION ALL
    SELECT f.id FROM folders f JOIN subtree s ON f.parent_folder_id = s.id
        WHERE f.workspace_id = $1 AND f.deleted_at IS NULL
)
SELECT ` + fileColumns + ` FROM files
WHERE workspace_id = $1 AND folder_id IN (SELECT id FROM subtree) AND deleted_at IS NULL
ORDER BY id ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID, folderID)
	if err != nil {
		return nil, fmt.Errorf("list files in subtree: %w", err)
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f := &File{}
		if err := rows.Scan(&f.ID, &f.WorkspaceID, &f.FolderID, &f.Name, &f.CurrentVersionID, &f.SizeBytes, &f.MimeType, &f.CreatedBy, &f.CreatedAt, &f.UpdatedAt, &f.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CreateFileVersion inserts a new version row. Ownership check and
// version-number computation run inside a single transaction, and the
// INSERT ... SELECT statement atomically picks the next version number so
// concurrent callers cannot collide on the (file_id, version_number)
// unique constraint.
//
// This entrypoint does not surface the idempotent-replay branch:
// it is reached only from server-internal paths (e.g. KChat room
// attachments) that mint a fresh v.ID via uuid.New() and therefore
// can never hit the ON CONFLICT path. The boolean from
// insertVersionTx is intentionally discarded here.
func (r *PostgresRepository) CreateFileVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create version: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := insertVersionTx(ctx, tx, workspaceID, v); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateVersionAndSetCurrent inserts a new version and points the file's
// current_version_id at it, all within a single transaction so partial
// failures cannot leave the file with an orphan version or a stale
// current_version_id.
func (r *PostgresRepository) CreateVersionAndSetCurrent(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin create+set version: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Same rationale as CreateFileVersion: the `fresh` boolean is
	// not surfaced because this entrypoint is server-internal and
	// mints a fresh v.ID on every call.
	if _, err := insertVersionTx(ctx, tx, workspaceID, v); err != nil {
		return err
	}
	const setQ = `
UPDATE files SET current_version_id = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, setQ, workspaceID, v.FileID, v.ID)
	if err != nil {
		return fmt.Errorf("set current version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

// ConfirmVersion inserts a new version, points the file's
// current_version_id at it, and updates the file's size_bytes, all in a
// single transaction. Used by the upload-confirm endpoint so a partial
// failure cannot leave the file pointing at a new version while still
// reporting the previous version's size.
//
// The boolean return distinguishes a fresh confirm (`true`) from an
// idempotent retry of an already-confirmed upload (`false`); see
// insertVersionTx for the semantics. ConfirmUpload uses this to
// avoid double-emitting activity logs, usage events, and post-
// upload job dispatches when a network-flaky client re-issues the
// same confirm call.
func (r *PostgresRepository) ConfirmVersion(ctx context.Context, workspaceID uuid.UUID, v *FileVersion) (bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin confirm version: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fresh, err := insertVersionTx(ctx, tx, workspaceID, v)
	if err != nil {
		return false, err
	}
	if !fresh {
		// Idempotent replay: the original confirm's transaction
		// already advanced files.current_version_id and size_bytes
		// to the correct values atomically with the version row
		// insert. Re-issuing the UPDATE here would be unsafe in two
		// distinct ways:
		//
		//   1. Version regression — between the original confirm and
		//      this retry, another caller may have advanced
		//      current_version_id to a newer version (V2). Blindly
		//      resetting it back to v.ID (V1) would silently roll
		//      back the file pointer and overwrite size_bytes with
		//      V1's size, discarding V2's changes.
		//
		//   2. Spurious updated_at — even when no concurrent V2
		//      exists, re-running the UPDATE bumps updated_at = now()
		//      for no logical change, polluting last-modified
		//      timestamps that downstream sync clients rely on for
		//      change detection.
		//
		// Both issues are resolved by simply not running the UPDATE
		// on the replay path. The first commit already did it.
		return false, tx.Commit(ctx)
	}
	// pending_upload_object_key is cleared in the same statement that
	// advances current_version_id so the orphan GC scan never sees a
	// confirmed row. A small race window between ConfirmVersion and the
	// GC reconciler is still safely handled by the orphan predicates on
	// DeletePendingOrphan, but clearing the column here is the cheap
	// path that prevents the GC from ever considering the row.
	const setQ = `
UPDATE files SET current_version_id = $3, size_bytes = $4,
                 pending_upload_object_key = NULL, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := tx.Exec(ctx, setQ, workspaceID, v.FileID, v.ID, v.SizeBytes)
	if err != nil {
		return false, fmt.Errorf("confirm version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// insertVersionTx verifies file ownership and inserts a new version
// row, atomically computing the next version number via INSERT ...
// SELECT. The insert is idempotent on the primary key: when a row
// with the requested v.ID already exists, the existing row is
// re-fetched into `v` and the call returns nil — making
// ConfirmUpload safe to retry across network interruptions or
// client crashes without creating duplicate version rows pointing
// at the same S3 object.
//
// Idempotency is bounded by identity: the existing row must belong
// to the same FileID and carry the same ObjectKey the caller is
// confirming. A mismatch (different file_id or object_key) yields
// ErrVersionConflict — that case can only arise from a UUID forge
// attempt or a serious programming error, so we fail closed rather
// than silently coalescing two different uploads.
//
// On the idempotent path, size_bytes / checksum / created_by /
// version_number etc. are reloaded from the stored row. We do NOT
// honour the retry caller's claimed size or checksum — the original
// commit is authoritative — so a retry cannot mutate the recorded
// upload metadata.
func insertVersionTx(ctx context.Context, tx pgx.Tx, workspaceID uuid.UUID, v *FileVersion) (bool, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	var existing int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`, workspaceID, v.FileID).Scan(&existing); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, fmt.Errorf("verify file ownership: %w", err)
	}
	const q = `
INSERT INTO file_versions (id, file_id, version_number, object_key, size_bytes, checksum, created_by)
SELECT $1, $2, COALESCE(MAX(version_number), 0) + 1, $3, $4, $5, $6 FROM file_versions WHERE file_id = $2
ON CONFLICT (id) DO NOTHING
RETURNING version_number, created_at, COALESCE(scan_status, ''), COALESCE(scan_detail, ''), scanned_at`
	err := tx.QueryRow(ctx, q, v.ID, v.FileID, v.ObjectKey, v.SizeBytes, v.Checksum, v.CreatedBy).
		Scan(&v.VersionNumber, &v.CreatedAt, &v.ScanStatus, &v.ScanDetail, &v.ScannedAt)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("insert file version: %w", err)
	}

	// Conflict: a row with this id already exists. Re-fetch it,
	// verify (file_id, object_key) match what the caller asked for,
	// and populate v from the stored row so the surrounding
	// ConfirmVersion transaction updates files.size_bytes /
	// current_version_id to the *original* values (not whatever
	// the retrying client claimed). Returning fresh=false lets the
	// ConfirmUpload handler skip side effects (activity log,
	// billing usage event, post-upload jobs) on the retry path.
	var (
		storedFileID    uuid.UUID
		storedObjectKey string
	)
	const reQ = `
SELECT file_id, object_key, version_number, size_bytes, checksum, created_by, created_at,
       COALESCE(scan_status, ''), COALESCE(scan_detail, ''), scanned_at
FROM file_versions WHERE id = $1`
	if err := tx.QueryRow(ctx, reQ, v.ID).Scan(
		&storedFileID, &storedObjectKey, &v.VersionNumber, &v.SizeBytes, &v.Checksum, &v.CreatedBy, &v.CreatedAt,
		&v.ScanStatus, &v.ScanDetail, &v.ScannedAt,
	); err != nil {
		return false, fmt.Errorf("re-fetch existing version: %w", err)
	}
	if storedFileID != v.FileID || storedObjectKey != v.ObjectKey {
		return false, ErrVersionConflict
	}
	return false, nil
}

// ListVersions returns every version of a file, newest first.
func (r *PostgresRepository) ListVersions(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*FileVersion, error) {
	// Ownership check: the file must belong to the workspace.
	var exists int
	if err := r.pool.QueryRow(ctx, `SELECT 1 FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`, workspaceID, fileID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("verify file workspace: %w", err)
	}
	const q = `
SELECT id, file_id, version_number, object_key, size_bytes, checksum, created_by, created_at,
       COALESCE(scan_status, ''), COALESCE(scan_detail, ''), scanned_at
FROM file_versions WHERE file_id = $1 ORDER BY version_number DESC`
	rows, err := r.pool.Query(ctx, q, fileID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}
	defer rows.Close()
	var out []*FileVersion
	for rows.Next() {
		v := &FileVersion{}
		if err := rows.Scan(&v.ID, &v.FileID, &v.VersionNumber, &v.ObjectKey, &v.SizeBytes, &v.Checksum, &v.CreatedBy, &v.CreatedAt, &v.ScanStatus, &v.ScanDetail, &v.ScannedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GetVersionByID returns a single version row joined against files so the
// lookup is scoped to a workspace without requiring callers to pass
// the parent file id. Used by handlers that already hold a file's
// current_version_id and want to avoid paging ListVersions.
func (r *PostgresRepository) GetVersionByID(ctx context.Context, workspaceID, versionID uuid.UUID) (*FileVersion, error) {
	const q = `
SELECT v.id, v.file_id, v.version_number, v.object_key, v.size_bytes, v.checksum, v.created_by, v.created_at,
       COALESCE(v.scan_status, ''), COALESCE(v.scan_detail, ''), v.scanned_at
FROM file_versions v
JOIN files f ON f.id = v.file_id
WHERE f.workspace_id = $1 AND v.id = $2 AND f.deleted_at IS NULL`
	v := &FileVersion{}
	if err := r.pool.QueryRow(ctx, q, workspaceID, versionID).Scan(
		&v.ID, &v.FileID, &v.VersionNumber, &v.ObjectKey, &v.SizeBytes, &v.Checksum, &v.CreatedBy, &v.CreatedAt,
		&v.ScanStatus, &v.ScanDetail, &v.ScannedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get version by id: %w", err)
	}
	return v, nil
}

// SetCurrentVersion points a file at one of its existing versions.
func (r *PostgresRepository) SetCurrentVersion(ctx context.Context, workspaceID, fileID, versionID uuid.UUID) error {
	const q = `
UPDATE files SET current_version_id = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`
	tag, err := r.pool.Exec(ctx, q, workspaceID, fileID, versionID)
	if err != nil {
		return fmt.Errorf("set current version: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// pgUniqueViolation is the SQLSTATE class for unique_violation. We
// match on the literal so we don't pull pgconn into the file package
// just for one constant.
const pgUniqueViolation = "23505"

// AddTag attaches a tag to a file. The (file_id, tag) unique index
// surfaces duplicate inserts as ErrTagAlreadyExists rather than a
// generic 500 so the HTTP layer can return 409. The file ownership
// check happens inside the same transaction so a concurrent
// soft-delete cannot leave an orphan tag pointing at a tombstoned
// file.
func (r *PostgresRepository) AddTag(ctx context.Context, workspaceID, fileID, createdBy uuid.UUID, tag string) (*Tag, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin add tag: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM files WHERE workspace_id = $1 AND id = $2 AND deleted_at IS NULL`, workspaceID, fileID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("verify file: %w", err)
	}

	t := &Tag{
		ID:          uuid.New(),
		FileID:      fileID,
		WorkspaceID: workspaceID,
		Tag:         tag,
		CreatedBy:   createdBy,
	}
	const q = `
INSERT INTO file_tags (id, file_id, workspace_id, tag, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING created_at`
	if err := tx.QueryRow(ctx, q, t.ID, t.FileID, t.WorkspaceID, t.Tag, t.CreatedBy).Scan(&t.CreatedAt); err != nil {
		// pgx returns *pgconn.PgError on SQL errors. Use string match
		// to detect the unique violation without taking a pgconn
		// dependency in this file.
		if strings.Contains(err.Error(), pgUniqueViolation) {
			return nil, ErrTagAlreadyExists
		}
		return nil, fmt.Errorf("insert tag: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit add tag: %w", err)
	}
	return t, nil
}

// RemoveTag deletes a single (file, tag) pair. Returns ErrNotFound
// when no row matches so the handler can map to 404 instead of 204.
func (r *PostgresRepository) RemoveTag(ctx context.Context, workspaceID, fileID uuid.UUID, tag string) error {
	const q = `
DELETE FROM file_tags
WHERE workspace_id = $1 AND file_id = $2 AND tag = $3`
	cmd, err := r.pool.Exec(ctx, q, workspaceID, fileID, tag)
	if err != nil {
		return fmt.Errorf("delete tag: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTagsByFile returns every tag attached to a file, alphabetically.
func (r *PostgresRepository) ListTagsByFile(ctx context.Context, workspaceID, fileID uuid.UUID) ([]*Tag, error) {
	const q = `
SELECT id, file_id, workspace_id, tag, created_by, created_at
FROM file_tags
WHERE workspace_id = $1 AND file_id = $2
ORDER BY tag ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID, fileID)
	if err != nil {
		return nil, fmt.Errorf("list tags by file: %w", err)
	}
	defer rows.Close()
	out := []*Tag{}
	for rows.Next() {
		t := &Tag{}
		if err := rows.Scan(&t.ID, &t.FileID, &t.WorkspaceID, &t.Tag, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTagsByWorkspace returns every tag in a workspace ordered by
// (tag, created_at). Used by admin UIs that want to surface popular
// tags across the org.
func (r *PostgresRepository) ListTagsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]*Tag, error) {
	const q = `
SELECT id, file_id, workspace_id, tag, created_by, created_at
FROM file_tags
WHERE workspace_id = $1
ORDER BY tag ASC, created_at ASC`
	rows, err := r.pool.Query(ctx, q, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list tags by workspace: %w", err)
	}
	defer rows.Close()
	out := []*Tag{}
	for rows.Next() {
		t := &Tag{}
		if err := rows.Scan(&t.ID, &t.FileID, &t.WorkspaceID, &t.Tag, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
