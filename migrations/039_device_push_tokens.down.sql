-- Reverse migration: native mobile push device tokens.
--
-- The RLS policy and the UNIQUE constraint's backing index drop
-- together with the table under DROP TABLE CASCADE, so explicit
-- DROP POLICY / DROP INDEX calls would be redundant.

DROP TABLE IF EXISTS device_push_tokens CASCADE;
