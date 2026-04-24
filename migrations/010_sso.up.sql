-- SSO: track the identity provider used to authenticate a user and the
-- provider's stable subject id (Google `sub`, Microsoft `oid`). Both
-- columns are nullable so password users continue to work unchanged.
-- The unique index is partial so multiple password-only rows without a
-- provider id don't collide.
ALTER TABLE users
    ADD COLUMN auth_provider TEXT,
    ADD COLUMN auth_provider_id TEXT;

CREATE UNIQUE INDEX idx_users_workspace_auth_provider
    ON users (workspace_id, auth_provider, auth_provider_id)
    WHERE auth_provider IS NOT NULL AND auth_provider_id IS NOT NULL;
