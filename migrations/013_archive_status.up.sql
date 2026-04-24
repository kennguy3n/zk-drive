-- archived_at stamps versions that the cold-archive worker has moved
-- to compressed archive storage. The hot object may still exist (for
-- fast restore); the column tells the admin UI which versions are
-- cold.
ALTER TABLE file_versions
    ADD COLUMN archived_at TIMESTAMPTZ;

CREATE INDEX idx_file_versions_archived ON file_versions(archived_at)
    WHERE archived_at IS NOT NULL;
