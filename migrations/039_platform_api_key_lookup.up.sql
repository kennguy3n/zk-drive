-- platform_api_keys.Authenticate originally loaded every active key and
-- bcrypt-compared the presented token against each one. That makes the
-- number of (expensive) bcrypt comparisons grow linearly with the key
-- count and, more importantly, lets an unauthenticated caller spraying
-- bogus "pk_" tokens force one bcrypt hash per stored key on every
-- request — a CPU-amplification vector independent of how few keys are
-- legitimately in use.
--
-- key_lookup is a deterministic SHA-256 digest of the plaintext key.
-- Because the plaintext carries 256 bits of entropy the digest is
-- effectively unguessable, so it is safe to index and equality-match on:
-- authentication becomes a single indexed row fetch followed by at most
-- ONE bcrypt comparison (kept as defense-in-depth on the matched row).
-- The bcrypt hash remains the stored verifier, so a database leak still
-- cannot recover usable keys.
--
-- The column is nullable to keep the migration online (no rewrite/backfill
-- needed). platform_api_keys is introduced in migration 036 and ships in
-- the same release as this migration, so in practice there are no
-- pre-existing rows; any key minted before key_lookup was populated would
-- have a NULL digest and must be re-minted to authenticate.
ALTER TABLE platform_api_keys ADD COLUMN key_lookup BYTEA;

-- Unique partial index: each active lookup digest maps to exactly one key,
-- so Authenticate can fetch the single candidate by digest. NULLs are
-- excluded so legacy/un-backfilled rows neither collide nor are matchable.
CREATE UNIQUE INDEX idx_platform_api_keys_lookup
    ON platform_api_keys(key_lookup)
    WHERE key_lookup IS NOT NULL;
