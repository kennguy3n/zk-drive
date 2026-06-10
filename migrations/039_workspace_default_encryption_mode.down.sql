-- Reverse 039_workspace_default_encryption_mode.
ALTER TABLE workspaces
  DROP CONSTRAINT IF EXISTS workspaces_default_encryption_mode_check;
ALTER TABLE workspaces
  DROP COLUMN IF EXISTS default_encryption_mode;
