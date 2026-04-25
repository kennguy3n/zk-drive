-- Per-workspace zk-object-fabric tenant credentials (Phase 4, task 2).
--
-- Each workspace maps 1:1 to a zk-object-fabric tenant. On workspace
-- creation, the signup handler best-effort calls the fabric console
-- API to mint a tenant + initial API key pair and stores them here.
-- The static S3_* env vars remain as the fallback for legacy
-- workspaces that predate this migration; the storage.ClientFactory
-- looks up this table first and falls back to the env-derived static
-- client when no row exists.
CREATE TABLE workspace_storage_credentials (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id),
    tenant_id TEXT NOT NULL,
    access_key TEXT NOT NULL,
    secret_key_encrypted TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    bucket TEXT NOT NULL,
    placement_policy_ref TEXT NOT NULL DEFAULT 'b2c_pooled_default',
    data_residency_country TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
