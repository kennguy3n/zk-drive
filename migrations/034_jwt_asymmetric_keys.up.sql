-- Asymmetric (ES256) JWT signing keys with rotation support.
--
-- zk-drive historically signed session tokens with a single
-- symmetric HS256 secret (JWT_SECRET). That secret has to be shared
-- by every process that verifies a token, so it cannot be rotated
-- without a coordinated restart, and any service that only needs to
-- *verify* tokens still has to hold the *signing* secret.
--
-- This table backs the migration to ES256 (ECDSA P-256) asymmetric
-- signing (internal/crypto.KeyManager):
--   - The private key signs tokens and is stored encrypted at rest
--     (AES-256-GCM via CREDENTIAL_ENCRYPTION_KEY, the same codec the
--     rest of the control plane uses for credentials at rest).
--   - The public key verifies tokens and is stored in clear PEM so a
--     future verify-only service can read it without the codec.
--
-- # Rotation
--
-- RotateKey() generates a fresh P-256 keypair, marks it active, and
-- flips the previously-active row to is_active=false (recording
-- rotated_at). Old rows are retained — never deleted — so tokens
-- signed by a now-retired key keep verifying until they expire. The
-- KeyManager loads every row for verification (keyed by the JWT `kid`
-- header) and signs only with the active row's key.
--
-- # Scope
--
-- workspace_id is nullable: NULL means a platform-wide key (the
-- common case — session tokens are platform-signed). A non-null
-- workspace_id reserves room for per-workspace signing keys without a
-- future migration. The partial unique index enforces "at most one
-- active key per scope", treating NULL (platform) as a single scope
-- via COALESCE to the nil UUID.

CREATE TABLE jwt_signing_keys (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL = platform-wide key. A non-null value scopes the key to a
    -- single workspace. No FK ON DELETE CASCADE: deleting a workspace
    -- must not silently drop a key that may still be verifying
    -- in-flight tokens; the row is harmless once its tokens expire.
    workspace_id              UUID REFERENCES workspaces(id) ON DELETE CASCADE,
    -- algorithm is stored so a future migration can introduce other
    -- curves/algs (e.g. EdDSA) without a schema change. Today only
    -- 'ES256' is written.
    algorithm                 TEXT NOT NULL DEFAULT 'ES256',
    -- PKIX/SPKI PEM ("-----BEGIN PUBLIC KEY-----"). Clear text: public
    -- keys are not secret and verify-only consumers need them.
    public_key_pem            TEXT NOT NULL,
    -- SEC1 EC private key PEM, encrypted with the credential codec
    -- (AES-256-GCM). BYTEA because the ciphertext is an opaque blob.
    private_key_pem_encrypted BYTEA NOT NULL,
    is_active                 BOOLEAN NOT NULL DEFAULT TRUE,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Set when the key is rotated out of the active slot. NULL while
    -- the key is active.
    rotated_at                TIMESTAMPTZ
);

-- At most one active key per scope. NULL workspace_id (platform-wide)
-- collapses to the nil UUID so two active platform keys collide,
-- while per-workspace active keys remain independent.
CREATE UNIQUE INDEX idx_jwt_signing_keys_active_scope
    ON jwt_signing_keys (COALESCE(workspace_id, '00000000-0000-0000-0000-000000000000'::uuid))
    WHERE is_active;

-- Hot path on boot / verification: load all keys for a scope.
CREATE INDEX idx_jwt_signing_keys_workspace
    ON jwt_signing_keys (workspace_id);
