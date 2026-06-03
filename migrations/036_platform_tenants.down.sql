-- Reverse 036_platform_tenants.
DROP INDEX IF EXISTS idx_platform_api_keys_active;
DROP TABLE IF EXISTS platform_api_keys;

ALTER TABLE workspaces DROP COLUMN IF EXISTS provisioned_by;
ALTER TABLE workspaces DROP COLUMN IF EXISTS suspension_reason;
ALTER TABLE workspaces DROP COLUMN IF EXISTS suspended_at;
