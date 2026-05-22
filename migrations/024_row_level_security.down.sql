-- Reverse of 024_row_level_security.up.sql.
--
-- Disables RLS on every tenant table, drops the tenant_isolation
-- policy, and removes the helper function. Tables continue to carry
-- workspace_id columns and indexes so the application's existing
-- explicit WHERE filters keep working unchanged.

DO $$
DECLARE
    t TEXT;
    all_tables TEXT[] := ARRAY[
        'workspaces',
        'users',
        'folders',
        'files',
        'file_versions',
        'file_previews',
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
    FOREACH t IN ARRAY all_tables LOOP
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', t);
    END LOOP;
END$$;

DROP FUNCTION IF EXISTS app_current_workspace_id();
