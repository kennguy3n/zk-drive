DROP INDEX IF EXISTS idx_file_versions_scan_status;
ALTER TABLE file_versions
    DROP COLUMN IF EXISTS scan_status,
    DROP COLUMN IF EXISTS scan_detail,
    DROP COLUMN IF EXISTS scanned_at;
