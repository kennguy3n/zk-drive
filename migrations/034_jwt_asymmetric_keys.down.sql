-- Reverse migration: asymmetric JWT signing keys.
--
-- Dropping the table also drops its indexes. Once removed, the
-- KeyManager finds no asymmetric keys and falls back to HS256
-- signing/verification using JWT_SECRET, so token issuance keeps
-- working — only tokens still signed by an ES256 key (if any were
-- issued before the rollback) stop verifying.

DROP TABLE IF EXISTS jwt_signing_keys CASCADE;
