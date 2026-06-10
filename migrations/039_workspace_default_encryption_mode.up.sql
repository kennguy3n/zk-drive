-- Workspace-level default encryption mode (Strict ZK as a default option).
--
-- Adds workspaces.default_encryption_mode: the encryption mode applied to
-- newly-created ROOT folders when the caller does not specify one. Child
-- folders always inherit their parent's mode (see internal/folder.Service),
-- so this knob only governs the top of each subtree.
--
-- Values mirror internal/folder's constants:
--   'managed_encrypted' (default) — server-side preview / scan / search are
--     enabled and the gateway manages keys. Matches prior behaviour.
--   'strict_zk' — every server-side processing path is disabled; file content
--     is opaque to the server (zero-knowledge). A "Secure Business" tier
--     workspace can flip its default here so every new folder is strict-ZK
--     unless an admin opts a specific folder back to managed.
--
-- A CHECK constraint pins the two recognised values at the database layer so
-- an out-of-band writer can't persist a mode the application would reject. The
-- DEFAULT backfills every existing row with 'managed_encrypted', preserving the
-- current behaviour for all workspaces created before this migration.
ALTER TABLE workspaces
  ADD COLUMN IF NOT EXISTS default_encryption_mode TEXT NOT NULL DEFAULT 'managed_encrypted';

ALTER TABLE workspaces
  DROP CONSTRAINT IF EXISTS workspaces_default_encryption_mode_check;

ALTER TABLE workspaces
  ADD CONSTRAINT workspaces_default_encryption_mode_check
  CHECK (default_encryption_mode IN ('managed_encrypted', 'strict_zk'));
