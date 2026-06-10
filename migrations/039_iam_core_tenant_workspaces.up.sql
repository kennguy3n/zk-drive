-- iam-core tenant -> zk-drive workspace mapping.
--
-- When zk-drive runs with iam-core (uneycom/iam-core) as its OAuth2 /
-- OIDC identity provider (IAM_CORE_ISSUER_URL set), every access token
-- carries the caller's iam-core tenant_id and org_id claims. This table
-- records the one-to-one mapping from an iam-core tenant (the
-- tenant_id + org_id pair) to the zk-drive workspace that backs it.
--
-- # Lifecycle
--
-- The row is created lazily the first time a user from a previously
-- unseen iam-core tenant authenticates: internal/iamcore.TenantMapper
-- auto-provisions a workspace (reusing internal/workspace) and inserts
-- the mapping under a transaction-scoped advisory lock so two
-- concurrent first-logins from the same tenant converge on a single
-- workspace rather than racing to create two. Subsequent logins resolve
-- the existing mapping (and an in-memory cache short-circuits the
-- lookup entirely at steady state).
--
-- # Key shape
--
-- Either claim may be empty for a given deployment (some iam-core
-- tenants are modelled purely by org_id, others by tenant_id), so both
-- columns are NOT NULL with an empty-string default rather than
-- nullable: a UNIQUE constraint treats two NULLs as distinct, which
-- would let the same tenant map to multiple workspaces. Storing '' for
-- the absent component keeps the UNIQUE(iam_tenant_id, iam_org_id)
-- constraint a true idempotency key. The application rejects a token
-- with BOTH components empty (fail closed) before it reaches this
-- table, so the all-empty row never occurs in practice.
--
-- # RLS
--
-- This is an infrastructure mapping table, not tenant data, and it is
-- only ever read/written by the iam-core middleware BEFORE the request
-- context is tenant-bound (so the app.workspace_id GUC is unset and the
-- `app_current_workspace_id() IS NULL` bypass branch applies). It still
-- carries the standard tenant_isolation policy keyed on workspace_id
-- for defence in depth and schema consistency (migration 024): should a
-- future code path ever query it under a bound workspace, it sees only
-- its own row.

CREATE TABLE iam_core_tenant_workspaces (
    iam_tenant_id TEXT        NOT NULL DEFAULT '' CHECK (length(iam_tenant_id) <= 255),
    iam_org_id    TEXT        NOT NULL DEFAULT '' CHECK (length(iam_org_id) <= 255),
    workspace_id  UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (iam_tenant_id, iam_org_id)
);

-- Reverse lookup (workspace -> iam-core tenant) backs ON DELETE CASCADE
-- cleanup and any future admin tooling that needs to find which tenant
-- a workspace belongs to. The forward lookup on (iam_tenant_id,
-- iam_org_id) is served by the primary key's backing index.
CREATE INDEX idx_iam_core_tenant_workspaces_workspace
    ON iam_core_tenant_workspaces (workspace_id);

ALTER TABLE iam_core_tenant_workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE iam_core_tenant_workspaces FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON iam_core_tenant_workspaces
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
