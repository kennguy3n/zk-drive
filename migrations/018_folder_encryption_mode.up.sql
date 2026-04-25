-- Per-folder encryption mode (Phase 4, task 4).
--
-- Folders default to 'managed_encrypted' so existing rows behave
-- exactly as before this migration. 'strict_zk' folders disable
-- server-side preview / scan / search at the worker layer and reject
-- cross-mode moves at the service layer.
ALTER TABLE folders
    ADD COLUMN encryption_mode TEXT NOT NULL DEFAULT 'managed_encrypted';
