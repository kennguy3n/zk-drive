DROP INDEX IF EXISTS idx_users_workspace_auth_provider;
ALTER TABLE users DROP COLUMN IF EXISTS auth_provider;
ALTER TABLE users DROP COLUMN IF EXISTS auth_provider_id;
