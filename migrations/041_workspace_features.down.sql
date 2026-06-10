-- Reverse migration: per-workspace feature overrides.
--
-- DROP TABLE CASCADE removes the RLS policy and the table in one step;
-- the explicit DROP POLICY would be redundant.

DROP TABLE IF EXISTS workspace_features CASCADE;
