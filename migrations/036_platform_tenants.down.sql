-- Reverse 036_platform_tenants.
-- The lookup_id UNIQUE index is dropped implicitly with the table.
DROP TABLE IF EXISTS platform_api_keys;

ALTER TABLE workspaces DROP COLUMN IF EXISTS provisioned_by;
ALTER TABLE workspaces DROP COLUMN IF EXISTS suspension_reason;
ALTER TABLE workspaces DROP COLUMN IF EXISTS suspended_at;
