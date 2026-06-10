-- Reverse migration: iam-core tenant -> workspace mapping.
--
-- The RLS policy and the secondary index drop together with the table
-- under DROP TABLE CASCADE, so explicit DROP POLICY / DROP INDEX calls
-- would be redundant.

DROP TABLE IF EXISTS iam_core_tenant_workspaces CASCADE;
