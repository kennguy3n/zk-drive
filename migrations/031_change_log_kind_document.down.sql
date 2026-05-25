-- Revert change_log.kind to the pre-document set. Aborts if any
-- existing rows have kind='document' (the down-migration deliberately
-- does NOT cascade-delete those rows; operators must decide whether
-- to truncate or backfill before downgrading).

ALTER TABLE change_log
    DROP CONSTRAINT change_log_kind_check;

ALTER TABLE change_log
    ADD CONSTRAINT change_log_kind_check
    CHECK (kind IN ('file', 'folder', 'permission'));
