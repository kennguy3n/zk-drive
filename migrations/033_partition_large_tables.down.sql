-- Reverse 033: convert the hash-partitioned activity_log, audit_log,
-- and change_log back into plain (non-partitioned) tables.
--
-- Strategy mirrors the up migration: build a non-partitioned twin,
-- copy every row back, drop the partitioned table (which drops all 64
-- partitions and frees the original names), rename the twin into
-- place, and recreate the primary keys / indexes / foreign keys /
-- CHECK constraints / RLS policies exactly as migrations 003-031 left
-- them. The change_log sequence object is detached before the drop
-- and re-attached afterwards so it (and its current value) survives.
--
-- As in the up migration, each table is LOCKed in ACCESS EXCLUSIVE mode
-- before the INSERT...SELECT copy so no row committed by a concurrent
-- writer between the copy snapshot and the DROP can be lost.
--
-- change_log is restored WITHOUT a tenant_isolation RLS policy: the up
-- migration added that policy as a new improvement, but before 033
-- (migration 029, after RLS migration 024) change_log had no RLS, so
-- faithfully reverting 033 must leave it unprotected again.

-- ===========================================================
-- activity_log
-- ===========================================================
LOCK TABLE activity_log IN ACCESS EXCLUSIVE MODE;

CREATE TABLE activity_log_plain (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL,
    user_id UUID NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID NOT NULL,
    metadata_json JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO activity_log_plain
    (id, workspace_id, user_id, action, resource_type, resource_id, metadata_json, created_at)
SELECT id, workspace_id, user_id, action, resource_type, resource_id, metadata_json, created_at
FROM activity_log;

DROP TABLE activity_log;
ALTER TABLE activity_log_plain RENAME TO activity_log;
ALTER INDEX activity_log_plain_pkey RENAME TO activity_log_pkey;

ALTER TABLE activity_log
    ADD CONSTRAINT activity_log_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id);
ALTER TABLE activity_log
    ADD CONSTRAINT activity_log_user_id_fkey FOREIGN KEY (user_id) REFERENCES users(id);

CREATE INDEX idx_activity_log_workspace ON activity_log(workspace_id, created_at DESC);

CREATE POLICY tenant_isolation ON activity_log
    USING (app_current_workspace_id() IS NULL
           OR workspace_id = app_current_workspace_id())
    WITH CHECK (app_current_workspace_id() IS NULL
                OR workspace_id = app_current_workspace_id());
ALTER TABLE activity_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE activity_log FORCE ROW LEVEL SECURITY;

-- ===========================================================
-- audit_log
-- ===========================================================
LOCK TABLE audit_log IN ACCESS EXCLUSIVE MODE;

CREATE TABLE audit_log_plain (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL,
    actor_id UUID,
    action VARCHAR(128) NOT NULL,
    resource_type VARCHAR(64),
    resource_id UUID,
    ip_address INET,
    user_agent TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO audit_log_plain
    (id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata, created_at)
SELECT id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata, created_at
FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_plain RENAME TO audit_log;
ALTER INDEX audit_log_plain_pkey RENAME TO audit_log_pkey;

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id);

CREATE INDEX idx_audit_log_workspace ON audit_log(workspace_id, created_at DESC);

CREATE POLICY tenant_isolation ON audit_log
    USING (app_current_workspace_id() IS NULL
           OR workspace_id = app_current_workspace_id())
    WITH CHECK (app_current_workspace_id() IS NULL
                OR workspace_id = app_current_workspace_id());
ALTER TABLE audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log FORCE ROW LEVEL SECURITY;

-- ===========================================================
-- change_log
-- ===========================================================
LOCK TABLE change_log IN ACCESS EXCLUSIVE MODE;

ALTER SEQUENCE change_log_sequence_seq OWNED BY NONE;

CREATE TABLE change_log_plain (
    sequence BIGINT PRIMARY KEY DEFAULT nextval('change_log_sequence_seq'),
    workspace_id UUID NOT NULL,
    actor_id UUID,
    kind TEXT NOT NULL,
    op TEXT NOT NULL,
    resource_id UUID NOT NULL,
    parent_id UUID,
    name TEXT,
    metadata JSONB,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT change_log_kind_check
        CHECK (kind IN ('file', 'folder', 'permission', 'document')),
    CONSTRAINT change_log_op_check
        CHECK (op IN ('create', 'update', 'rename', 'move', 'delete'))
);

INSERT INTO change_log_plain
    (sequence, workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata, occurred_at)
SELECT sequence, workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata, occurred_at
FROM change_log;

DROP TABLE change_log;
ALTER TABLE change_log_plain RENAME TO change_log;
ALTER INDEX change_log_plain_pkey RENAME TO change_log_pkey;
ALTER SEQUENCE change_log_sequence_seq OWNED BY change_log.sequence;

ALTER TABLE change_log
    ADD CONSTRAINT change_log_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE change_log
    ADD CONSTRAINT change_log_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX idx_change_log_workspace_sequence
    ON change_log(workspace_id, sequence);
CREATE INDEX idx_change_log_workspace_occurred_at
    ON change_log(workspace_id, occurred_at DESC);
