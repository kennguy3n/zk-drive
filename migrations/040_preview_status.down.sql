DROP INDEX IF EXISTS idx_file_versions_preview_failed;

ALTER TABLE file_versions
    DROP COLUMN IF EXISTS preview_status,
    DROP COLUMN IF EXISTS preview_detail;
