-- Extend change_log.kind to include 'document' so collab document
-- mutations (create / rename / delete / change_collab_mode) flow
-- through the same workspace-wide mutation stream that backs the
-- desktop sync SDK. Sync clients only need to learn one new kind
-- value (the existing replay path is identical).
--
-- Adding 'document' here keeps the change_log.op CHECK unchanged:
-- documents only ever produce create / rename / update / delete ops
-- (no 'move' yet because the P2a iteration does not implement
-- cross-folder document moves; that lands in a later PR once the
-- snapshot re-encryption path for moves is finalised).
--
-- The DROP CONSTRAINT / ADD CONSTRAINT pair is the standard
-- Postgres pattern for editing a CHECK in place. It briefly takes
-- an ACCESS EXCLUSIVE on change_log; the table is small (one row
-- per workspace mutation) and the migration window is brief.

ALTER TABLE change_log
    DROP CONSTRAINT change_log_kind_check;

ALTER TABLE change_log
    ADD CONSTRAINT change_log_kind_check
    CHECK (kind IN ('file', 'folder', 'permission', 'document'));
