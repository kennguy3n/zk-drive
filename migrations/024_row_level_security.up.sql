-- Row-level security for defence-in-depth tenant isolation.
--
-- Every existing query already filters on `workspace_id = $1`, so
-- enabling RLS does not change application behaviour for the happy
-- path. The point of this migration is to add a safety net: if a
-- future bug omits the WHERE clause (or a new endpoint forgets to
-- thread the tenant id), Postgres still returns zero rows for the
-- wrong tenant rather than silently leaking data.
--
-- Tenant scoping is keyed on the `app.workspace_id` GUC, populated by
-- the pgxpool PrepareConn hook in internal/database/postgres.go
-- before any application query runs. When the GUC is empty / unset —
-- migrations, background workers without a request context, and the
-- pre-auth handlers (login, signup, public share-link resolution) —
-- the policies fall back to "no restriction" so cross-cutting paths
-- keep working unchanged. The helper function
-- `app_current_workspace_id()` returns NULL for the bypass case so
-- policies read clearly:
--
--   USING (
--       app_current_workspace_id() IS NULL
--       OR workspace_id = app_current_workspace_id()
--   )
--
-- file_versions and file_previews are scoped through their parent
-- files row because they don't carry a redundant workspace_id
-- column. A future migration could denormalise workspace_id onto
-- them to collapse the EXISTS subquery; performance has been within
-- budget on the current schema and the EXISTS keeps the storage
-- footprint smaller.
--
-- The migration creates a single `tenant_isolation` policy per table
-- with both USING and WITH CHECK clauses so the same predicate
-- applies to SELECT/UPDATE/DELETE (USING) and INSERT/UPDATE (WITH
-- CHECK). FORCE ROW LEVEL SECURITY is set so the table owner role
-- (used by the app in production) is also subject to the policy —
-- without FORCE, owners bypass RLS by default. Superusers always
-- bypass RLS regardless of FORCE; production deployments therefore
-- connect the app as a non-superuser role. Tests cover the
-- FORCE/non-superuser path explicitly (tests/integration/rls_test.go).

CREATE OR REPLACE FUNCTION app_current_workspace_id()
RETURNS UUID
LANGUAGE SQL
STABLE
AS $$
    SELECT NULLIF(current_setting('app.workspace_id', true), '')::UUID
$$;

COMMENT ON FUNCTION app_current_workspace_id() IS
'Returns the tenant UUID bound to the current connection via the app.workspace_id GUC, or NULL when unset. Used by all tenant_isolation row-level-security policies as the canonical bypass-or-match decision point.';

-- Direct tenant tables: each row carries a workspace_id column that
-- is the authoritative tenant key. The DO block keeps the policy
-- definitions identical across tables so a future audit can confirm
-- a single source of truth.
DO $$
DECLARE
    t TEXT;
    direct_tables TEXT[] := ARRAY[
        'users',
        'folders',
        'files',
        'permissions',
        'activity_log',
        'share_links',
        'guest_invites',
        'client_rooms',
        'notifications',
        'audit_log',
        'retention_policies',
        'file_tags',
        'workspace_plans',
        'usage_events',
        'workspace_storage_credentials',
        'kchat_room_folders'
    ];
BEGIN
    FOREACH t IN ARRAY direct_tables LOOP
        EXECUTE format(
            'CREATE POLICY tenant_isolation ON %I '
            'USING (app_current_workspace_id() IS NULL '
            '       OR workspace_id = app_current_workspace_id()) '
            'WITH CHECK (app_current_workspace_id() IS NULL '
            '            OR workspace_id = app_current_workspace_id())',
            t
        );
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    END LOOP;
END$$;

-- workspaces: the id column IS the tenant key, so policies match on
-- id rather than workspace_id.
CREATE POLICY tenant_isolation ON workspaces
    USING (
        app_current_workspace_id() IS NULL
        OR id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR id = app_current_workspace_id()
    );
ALTER TABLE workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces FORCE ROW LEVEL SECURITY;

-- file_versions: tenant inferred via parent files.workspace_id. The
-- EXISTS subquery is evaluated against the files table's own RLS
-- policy, so even an explicit JOIN by id from the wrong tenant
-- returns zero rows.
CREATE POLICY tenant_isolation ON file_versions
    USING (
        app_current_workspace_id() IS NULL
        OR EXISTS (
            SELECT 1
            FROM files
            WHERE files.id = file_versions.file_id
              AND files.workspace_id = app_current_workspace_id()
        )
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR EXISTS (
            SELECT 1
            FROM files
            WHERE files.id = file_versions.file_id
              AND files.workspace_id = app_current_workspace_id()
        )
    );
ALTER TABLE file_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_versions FORCE ROW LEVEL SECURITY;

-- file_previews: tenant inferred via parent files row (same pattern
-- as file_versions; previews always reference files directly).
CREATE POLICY tenant_isolation ON file_previews
    USING (
        app_current_workspace_id() IS NULL
        OR EXISTS (
            SELECT 1
            FROM files
            WHERE files.id = file_previews.file_id
              AND files.workspace_id = app_current_workspace_id()
        )
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR EXISTS (
            SELECT 1
            FROM files
            WHERE files.id = file_previews.file_id
              AND files.workspace_id = app_current_workspace_id()
        )
    );
ALTER TABLE file_previews ENABLE ROW LEVEL SECURITY;
ALTER TABLE file_previews FORCE ROW LEVEL SECURITY;
