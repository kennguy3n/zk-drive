-- Customer-managed key (CMK) URI per workspace (Phase 4, task 6).
--
-- A non-empty cmk_uri instructs zk-object-fabric to use a customer-
-- managed key for the workspace's tenant rather than the gateway's
-- default. The URI scheme dispatches to the underlying provider
-- (arn:aws:kms:..., kms://..., vault://..., transit://...). An empty
-- value preserves the gateway-default behaviour and is the schema's
-- default so existing rows behave exactly as before this migration.
ALTER TABLE workspace_storage_credentials
    ADD COLUMN cmk_uri TEXT NOT NULL DEFAULT '';
