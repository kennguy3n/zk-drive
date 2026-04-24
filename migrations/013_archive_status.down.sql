DROP INDEX IF EXISTS idx_file_versions_archived;
ALTER TABLE file_versions DROP COLUMN IF EXISTS archived_at;
