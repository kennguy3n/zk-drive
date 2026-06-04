-- Reverse migration: Web Push subscriptions.
--
-- The RLS policy and the UNIQUE constraint's backing index drop
-- together with the table under DROP TABLE CASCADE, so explicit
-- DROP POLICY / DROP INDEX calls would be redundant.

DROP TABLE IF EXISTS webpush_subscriptions CASCADE;
