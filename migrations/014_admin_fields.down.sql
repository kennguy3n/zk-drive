DROP INDEX IF EXISTS idx_users_workspace_active;
ALTER TABLE users DROP COLUMN IF EXISTS last_login_at;
ALTER TABLE users DROP COLUMN IF EXISTS deactivated_at;
