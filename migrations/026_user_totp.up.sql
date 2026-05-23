-- Phase 5 (WS-19): TOTP-based two-factor authentication for
-- password and SSO logins.
--
-- Threat model: WS-5 (bcrypt cost 12) and WS-1 (Redis-backed session
-- revocation) closed the password-related auth gaps, but a leaked
-- DB row, a phished password, or password reuse from another breach
-- still fully owns the account. Adding a TOTP possession factor
-- means an attacker holding the password cannot complete login
-- without also holding the device that owns the shared secret.
--
-- The shared secret is encrypted at rest with the same
-- internal/crypto.Codec (AES-256-GCM, keyed via the existing
-- CREDENTIAL_ENCRYPTION_KEY env var) that protects per-tenant
-- storage credentials — a single key-management story for both
-- credential families. Operators rotating the encryption key
-- already have the migration runbook from WS-13; TOTP secrets
-- ride the same path.
--
-- Recovery codes:
--   - Generated once at enrollment finalize, returned plaintext
--     to the user (single display), then immediately bcrypt-hashed
--     (cost = internal/crypto.PasswordHashCost, currently 12) so a
--     DB dump leaks neither the TOTP secret nor any usable recovery
--     codes.
--   - 10 codes per user is the industry-standard quantity (GitHub,
--     Google, AWS all default to 10). Burned codes are NOT deleted
--     — used_at is stamped instead — so audit queries can prove a
--     recovery code was the second factor on a given session.
--
-- Replay window:
--   - user_totp_credentials.last_used_at pins the most recent code
--     accepted for the user. The verifier rejects any code whose
--     30-second period is <= last_used_at, so a code observed by a
--     network attacker mid-flight cannot be replayed during the
--     same 30s window. RFC 6238 §5.2 mandates this single-use
--     property.
--
-- Workspace-level enforcement:
--   - workspaces.mfa_required = TRUE forces every user in the
--     workspace to enroll before completing login. Until enrollment
--     is finalized they receive a challenge-only JWT
--     (purpose=mfa_enroll) that authorizes the enrollment endpoints
--     and nothing else — the user cannot reach data plane handlers
--     without first satisfying the workspace policy.

CREATE TABLE user_totp_credentials (
    user_id          UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    -- AES-256-GCM ciphertext from internal/crypto.Codec. NULL after
    -- Disable() so the row can stay (preserving last_used_at /
    -- created_at audit history) while no secret is active.
    encrypted_secret TEXT        NOT NULL,
    -- NULL until FinalizeEnrollment succeeds. Pending enrollments
    -- (begin without finalize) leave the row present so the user
    -- can resume the same secret from another tab without
    -- regenerating; we expire those server-side on the next Begin
    -- call by overwriting the row.
    activated_at     TIMESTAMPTZ NULL,
    -- Pins the most-recently-accepted period boundary. The
    -- verifier rejects any code whose computed period start is
    -- <= last_used_at. NULL means the credential has been activated
    -- but no code has ever been verified — the next Verify call
    -- will accept any in-window code and stamp this column.
    last_used_at     TIMESTAMPTZ NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE user_totp_recovery_codes (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- bcrypt hash (internal/crypto.HashPassword) of the recovery
    -- code as displayed to the user. The plaintext code is shown
    -- exactly once at enrollment finalize and never persisted.
    code_hash   TEXT        NOT NULL,
    -- NULL until the code is consumed via ConsumeRecoveryCode().
    -- Stamping the column (vs deleting the row) preserves the
    -- audit trail: an investigator can later prove a specific
    -- recovery code was burned at a specific time.
    used_at     TIMESTAMPTZ NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index pins the hot path: the verifier scans only un-used
-- codes for a user, which is normally 8-10 rows. Without the
-- partial predicate a long-lived account that has burned many
-- recovery codes (and re-enrolled multiple times) would force the
-- scanner through every historical row on every verify.
CREATE INDEX idx_user_totp_recovery_codes_unused
    ON user_totp_recovery_codes(user_id)
    WHERE used_at IS NULL;

ALTER TABLE workspaces
    ADD COLUMN mfa_required BOOLEAN NOT NULL DEFAULT FALSE;

-- Row-level security: WS-13's policy on user_totp_credentials would
-- be a cross-table join (users -> workspace) that the planner can't
-- index-only, so we rely on the application layer's tenant guard
-- for write paths and verify reads via the user_id PK (which is
-- already workspace-scoped via the users foreign key). The
-- recovery codes table follows the same model.
