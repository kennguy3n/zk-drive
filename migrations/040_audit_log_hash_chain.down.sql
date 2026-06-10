-- Reverse 040_audit_log_hash_chain.up.sql.
DROP INDEX IF EXISTS idx_audit_log_workspace_seq;
DROP TABLE IF EXISTS audit_log_chain_head;
ALTER TABLE audit_log
    DROP COLUMN IF EXISTS entry_hash,
    DROP COLUMN IF EXISTS prev_hash,
    DROP COLUMN IF EXISTS seq;
