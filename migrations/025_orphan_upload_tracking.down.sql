-- Reverse of 025_orphan_upload_tracking.up.sql.
--
-- Drops the partial index first (it depends on the column predicate
-- which would otherwise be silently dropped by ALTER TABLE DROP
-- COLUMN, but explicit cleanup keeps the operation visible in DDL
-- review tools).

DROP INDEX IF EXISTS idx_files_pending_orphan;
ALTER TABLE files DROP COLUMN IF EXISTS pending_upload_object_key;
