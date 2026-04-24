-- Phase 2: virus scan status on file versions.
--
-- scan_status tracks the async ClamAV scan lifecycle for each uploaded
-- version:
--   pending      — newly uploaded, scan job enqueued but not yet run
--   scanning     — worker picked up the job, scan in progress
--   clean        — ClamAV reported OK; downloads are unblocked
--   quarantined  — ClamAV detected a threat; downloads refuse to
--                  generate a presigned URL and the admins are
--                  notified via the notification service
--
-- scan_detail captures the raw ClamAV response (signature name for
-- quarantined versions, error text for failed scans) so admins can
-- audit without rummaging through worker logs.

ALTER TABLE file_versions
    ADD COLUMN scan_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (scan_status IN ('pending', 'scanning', 'clean', 'quarantined')),
    ADD COLUMN scan_detail TEXT,
    ADD COLUMN scanned_at TIMESTAMPTZ;

CREATE INDEX idx_file_versions_scan_status ON file_versions(scan_status);
