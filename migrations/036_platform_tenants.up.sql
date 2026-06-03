-- Platform control-plane: tenant lifecycle + super-API authentication.
--
-- This migration backs the admin super-API (api/platform, internal/platform)
-- that operations uses to provision, suspend, and reconcile the fleet of
-- workspaces (tenants) without going through the per-workspace JWT surface.
--
-- # workspaces lifecycle columns
--
-- suspended_at / suspension_reason record an operator-initiated suspension.
-- When suspended_at IS NOT NULL the workspace-loading middleware short-circuits
-- every authenticated request with 503 Service Unavailable so a suspended
-- tenant cannot read or mutate data while the account is on hold (non-payment,
-- abuse investigation, etc.). Resuming clears both columns.
--
-- provisioned_by records how the workspace came into existence:
--   'manual' — created through the normal /api/auth/signup flow (default for
--              pre-existing rows is NULL, which the API renders as 'manual').
--   'api'    — provisioned via POST /api/platform/workspaces.
--   'stripe' — provisioned by a billing-driven flow.
-- Kept as free-form TEXT (no CHECK / ENUM) so adding a provisioning source is
-- a code-only change, mirroring the tier / event_type conventions elsewhere.
ALTER TABLE workspaces ADD COLUMN suspended_at TIMESTAMPTZ;
ALTER TABLE workspaces ADD COLUMN suspension_reason TEXT;
ALTER TABLE workspaces ADD COLUMN provisioned_by TEXT;

-- platform_api_keys authenticates the platform super-API. Keys are minted
-- once (the plaintext is returned to the operator a single time) and only the
-- bcrypt hash is persisted, so a database leak cannot recover usable keys.
--
-- permissions is a coarse capability set checked by the platform-auth
-- middleware, e.g. {'tenant:read','tenant:write','tenant:suspend'}. Stored as a
-- TEXT[] so adding a capability needs no migration.
--
-- last_used_at is refreshed (best-effort) on every successful authentication
-- so operators can spot stale keys; revoked_at soft-revokes a key while keeping
-- the row for audit. There is no workspace_id — these keys are global to the
-- deployment and deliberately NOT subject to the tenant_isolation RLS policies
-- in migration 024.
CREATE TABLE platform_api_keys (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash     BYTEA NOT NULL,
    label        TEXT NOT NULL,
    permissions  TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- Hot path on every platform-API request is "list active keys to match the
-- presented secret against". The partial index keeps that scan tight as
-- revoked keys accumulate.
CREATE INDEX idx_platform_api_keys_active ON platform_api_keys(id) WHERE revoked_at IS NULL;
