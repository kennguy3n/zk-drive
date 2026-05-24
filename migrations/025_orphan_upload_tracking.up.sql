-- Track presigned-PUT object keys so the orphan garbage collector
-- can reclaim S3 objects that were uploaded but never confirmed.
--
-- Background: api/drive/upload.go:UploadURL generates a versionID
-- locally, signs it into the S3 presigned PUT URL, and hands both
-- the URL and the derived object_key back to the client without
-- persisting them anywhere. If the client successfully PUTs the
-- bytes but never returns to call ConfirmUpload — or returns and
-- ConfirmUpload is rejected (e.g. quota overage, tenant suspended,
-- network drop) — the S3 object is stranded with no DB row pointing
-- at it. Operators pay for that storage indefinitely and there is
-- no way to identify the orphan from the metadata side because the
-- generated versionID is lost the moment the handler returns.
--
-- This column records the presigned-PUT object_key on the file row
-- itself, so the GC reconciler can:
--   1. List files where current_version_id IS NULL AND
--      pending_upload_object_key IS NOT NULL AND created_at older
--      than the configured cooldown,
--   2. Issue a best-effort DeleteObject against the recorded key,
--   3. Delete the orphan file row.
--
-- ConfirmUpload clears the column when the version row is inserted,
-- so confirmed files never carry stale pending keys. The column is
-- nullable because legacy file rows from before this migration
-- have no recorded key and are intentionally invisible to the GC
-- scan (they would need a separate ListObjects-based cleanup pass,
-- out of scope for this migration).

ALTER TABLE files ADD COLUMN pending_upload_object_key TEXT;

-- Partial index pinning the GC reconciler's hot path: it only ever
-- scans rows with a pending key, which should be a tiny fraction of
-- the table during steady-state (presign window is 15 minutes; only
-- orphaned-by-failure rows persist beyond that).
CREATE INDEX idx_files_pending_orphan
    ON files(workspace_id, created_at)
    WHERE pending_upload_object_key IS NOT NULL
      AND current_version_id IS NULL
      AND deleted_at IS NULL;
