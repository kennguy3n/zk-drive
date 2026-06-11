-- Reverse migration: worker liveness heartbeats.
--
-- The PRIMARY KEY's backing index drops together with the table, so
-- an explicit DROP INDEX would be redundant.

DROP TABLE IF EXISTS worker_heartbeats CASCADE;
