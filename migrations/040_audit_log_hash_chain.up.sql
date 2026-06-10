-- 6.6 Audit-log integrity: tamper-evident HMAC hash chain.
--
-- Every audit_log row gains a monotonically increasing per-workspace
-- sequence number, the previous row's HMAC (prev_hash) and its own
-- HMAC (entry_hash). entry_hash = HMAC-SHA256(key, seq || prev_hash ||
-- immutable row fields), so the rows form a hash chain: altering,
-- deleting, or inserting any row breaks the link to its successor.
-- The HMAC key is held by the application (derived from an env secret,
-- never stored in the database), so even a DB admin who can write
-- arbitrary rows cannot forge a consistent chain.
ALTER TABLE audit_log
    ADD COLUMN IF NOT EXISTS seq        BIGINT,
    ADD COLUMN IF NOT EXISTS prev_hash  BYTEA,
    ADD COLUMN IF NOT EXISTS entry_hash BYTEA;

-- One row per workspace holding the latest sequence number and head
-- hash. Kept SEPARATE from audit_log (the requirement) so an external
-- verifier can periodically snapshot (workspace_id, seq, head_hash)
-- and later detect a wholesale rewrite of the live log: even an
-- attacker who recomputes every per-row hash to be internally
-- consistent cannot match a head_hash a third party already retained.
CREATE TABLE IF NOT EXISTS audit_log_chain_head (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    seq          BIGINT      NOT NULL,
    head_hash    BYTEA       NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unique per-workspace sequence. workspace_id is the hash-partition
-- key of audit_log (migration 033), so a partitioned unique index must
-- include it. NULL seqs (any pre-existing rows from before this
-- migration) are distinct under SQL NULL semantics, so the index only
-- constrains the chained rows written from here on; a tamper that
-- duplicates a row's seq trips the constraint instead of silently
-- forking the chain.
CREATE UNIQUE INDEX IF NOT EXISTS idx_audit_log_workspace_seq
    ON audit_log (workspace_id, seq);
