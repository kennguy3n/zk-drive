-- Reverse of 035_ip_allowlist.up.sql.
--
-- Drop the table (the tenant_isolation policy and the
-- uq_ip_allowlist_ws_cidr UNIQUE constraint/index are implicitly
-- dropped with it), then remove the
-- workspaces.ip_allowlist_enabled column. workspace_ip_allowlist FKs
-- workspaces/users, so it must go before either parent is touched —
-- but here only a column on workspaces is removed, which is safe
-- once the dependent table is gone.

DROP TABLE IF EXISTS workspace_ip_allowlist;

ALTER TABLE workspaces DROP COLUMN IF EXISTS ip_allowlist_enabled;
