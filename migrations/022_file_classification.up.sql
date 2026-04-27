-- Phase 4: per-file classification column.
--
-- Populated by the drive.classify.file worker job (see internal/classify).
-- NULL means the file has not been classified yet (or was skipped because
-- it lives in a strict-ZK folder where the server has no plaintext).
ALTER TABLE files ADD COLUMN IF NOT EXISTS classification TEXT DEFAULT NULL;
