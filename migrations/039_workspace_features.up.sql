-- Per-workspace feature overrides for progressive feature disclosure.
--
-- ZK Drive gates UI and server features behind a workspace's billing tier
-- (free/starter → business → secure_business). The tier defaults live in
-- code (internal/feature/flags.go) so adding a feature is a code-only
-- change. This table stores the EXCEPTIONS to those defaults: an explicit
-- per-workspace override that turns a single feature on or off regardless
-- of the workspace's tier.
--
-- Why a table rather than tier-only gating:
--   - Beta access: flip a Secure-Business feature on for one Business
--     workspace without changing their plan or billing.
--   - Contractual carve-outs: a customer that bought an add-on à la carte.
--   - Incident kill-switch: disable a misbehaving feature for one tenant
--     without a deploy or a tier downgrade.
--
-- The effective state of a feature is resolved in
-- internal/feature/service.go as: override (this table) if present, else
-- the tier default, else false (unknown features are disabled —
-- fail-closed).
--
-- # Schema
--
-- (workspace_id, feature) is the natural key — at most one override per
-- feature per workspace — so it is the composite PRIMARY KEY. `enabled`
-- carries the override value. updated_by records the admin who set it (no
-- ON DELETE CASCADE on the user FK so removing a user doesn't silently
-- erase the override audit trail); updated_at is maintained explicitly by
-- the repository on every upsert, matching the rest of the schema (no
-- shared trigger function exists; see migrations 001, 002, 028).
--
-- feature is stored as TEXT (no CHECK / ENUM) so adding or retiring a
-- feature key is a code-only change. The repository ignores rows whose
-- key the running build no longer recognises, so a stale override can
-- never surface in the API.
--
-- # RLS
--
-- Uses the same `tenant_isolation` pattern as the rest of the schema
-- (migration 024): a query that has set the app.workspace_id GUC sees
-- only its own workspace's rows; a connection that has NOT set it (the
-- bypass branch, app_current_workspace_id() IS NULL) sees everything,
-- which the GET /api/features handler relies on indirectly via the
-- tenant-scoped pool and which lets an operator script manage overrides
-- fleet-wide.
CREATE TABLE workspace_features (
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    feature      TEXT        NOT NULL CHECK (feature <> ''),
    enabled      BOOLEAN     NOT NULL,
    updated_by   UUID        REFERENCES users(id),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, feature)
);

-- The only hot query is "load every override for this workspace"
-- (GET /api/features on login). The PRIMARY KEY's leading column
-- (workspace_id) already covers that prefix scan, so no extra index is
-- needed.

ALTER TABLE workspace_features ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_features FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workspace_features
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
