-- Hash-partition the highest-growth append-only tables by
-- workspace_id for 5,000-tenant scale.
--
-- Motivation
-- ----------
-- activity_log, audit_log, and change_log are append-only and grow
-- without bound (activity_log fastest, then change_log's sync feed,
-- then the compliance-critical audit_log). At 5,000 tenants a single
-- heap + B-tree per table becomes the dominant maintenance cost:
-- autovacuum scans, index bloat, and (workspace_id, time) range scans
-- all degrade as the table crosses tens-to-hundreds of millions of
-- rows. Hash-partitioning by workspace_id spreads each table across
-- 64 partitions (~78 tenants' data per partition at 5,000 tenants),
-- so autovacuum, index maintenance, and partition pruning all operate
-- on a 1/64th slice.
--
-- Why partition pruning is automatic
-- -----------------------------------
-- Every production query against these tables already filters on
-- `workspace_id = $1` (the RLS tenant_isolation policy from migration
-- 024 makes that the load-bearing predicate). A query with an
-- equality on the hash partition key prunes to the single partition
-- holding that workspace, so the partitioning is transparent to the
-- application: no Go changes are required (Postgres routes INSERTs and
-- prunes SELECTs by the parent table name).
--
-- Why 64 hash partitions
-- ----------------------
-- 64 is a good balance for the 5,000-tenant target: large enough that
-- each partition stays small and autovacuum-friendly, small enough
-- that planning overhead and the per-partition file count stay modest.
-- Powers of two also let a future MODULUS split (64 -> 128) re-use the
-- existing remainders cleanly.
--
-- Conversion strategy (per table, all inside this migration's single
-- transaction — the migrate runner wraps each file in a tx):
--   1. LOCK the old table in ACCESS EXCLUSIVE mode up-front. This is
--      load-bearing for correctness, not just statement ordering: the
--      copy in step 3 is INSERT...SELECT, and that SELECT only takes an
--      ACCESS SHARE lock, which is compatible with the ROW EXCLUSIVE
--      lock concurrent INSERTs hold. Without an explicit exclusive
--      lock, another session could COMMIT a new row AFTER the SELECT's
--      snapshot but BEFORE the DROP in step 4 — that row would then be
--      destroyed by the DROP and lost forever. Taking ACCESS EXCLUSIVE
--      first blocks all concurrent writers for the whole copy, so the
--      set of rows the SELECT sees is exactly the set the DROP removes;
--      no write can slip through the gap.
--   2. CREATE a new `<t>_partitioned` parent PARTITION BY HASH
--      (workspace_id) with 64 partitions named `<t>_pNN`.
--   3. Copy every row from the old table into the new parent (tuple
--      routing distributes rows across partitions).
--   4. DROP the old table — this frees the original index / constraint
--      / policy names so the partitioned table can reuse them exactly.
--   5. RENAME the new parent to the original name.
--   6. Recreate the primary key (now including workspace_id, as a hash
--      partition's PK must contain the partition key), the secondary
--      indexes, the foreign keys, the CHECK constraints, and the RLS
--      policies under their original names.
--
-- Operational note (large tables)
-- -------------------------------
-- The copy is a single in-transaction INSERT...SELECT, so for a table
-- with hundreds of millions of rows it holds the ACCESS EXCLUSIVE lock
-- (all writes blocked) for the full copy and generates WAL for the
-- entire dataset. Run this migration inside a maintenance window /
-- write quiesce. If any of these tables ever outgrows what a window can
-- absorb, replace this copy-drop-rename with an online strategy
-- (pg_partman, logical replication, or chunked CREATE TABLE ... ATTACH
-- PARTITION) rather than a single bulk copy. At current volumes the
-- three logs copy comfortably within a maintenance window.
--
-- Primary-key note
-- ----------------
-- A hash-partitioned table's primary key (and every unique
-- constraint) MUST include the partition key. So:
--   activity_log : PK (id)        -> PK (id, workspace_id)
--   audit_log    : PK (id)        -> PK (id, workspace_id)
--   change_log   : PK (sequence)  -> PK (sequence, workspace_id)
-- `id` / `sequence` remain effectively unique in practice (id is a
-- random uuid; sequence comes from one shared sequence object — see
-- below), the composite PK just adds workspace_id to satisfy the
-- partitioning requirement.
--
-- change_log.sequence stays globally unique
-- -----------------------------------------
-- change_log.sequence is the global monotonic sync cursor (BIGSERIAL,
-- backed by the single sequence object change_log_sequence_seq). It
-- MUST stay globally unique across all partitions, NOT reset per
-- partition. We achieve that by reusing the EXISTING sequence object
-- as the column DEFAULT on the partitioned parent: all partitions
-- inherit the parent's `nextval(change_log_sequence_seq)` default, so
-- every INSERT — regardless of which partition it routes to — draws
-- from one shared counter. We detach the sequence's ownership before
-- dropping the old table (otherwise DROP TABLE would cascade-drop the
-- sequence) and re-attach it to the new column afterwards. Copied rows
-- keep their original sequence values; the sequence's last_value is
-- untouched, so future nextval() values stay above every copied row.
--
-- file_versions is intentionally NOT partitioned here — see the PR
-- description. Unlike the three log tables it has no workspace_id
-- column, is the target of two foreign keys (files.current_version_id
-- and file_previews.version_id reference file_versions(id)), and its
-- hot-path INSERT uses `ON CONFLICT (id)`. A hash-partitioned table
-- cannot expose a unique constraint on `id` alone, so all three would
-- break and require Go + cross-table FK changes that this task scopes
-- out ("No Go code changes needed").

-- ===========================================================
-- activity_log
-- ===========================================================
LOCK TABLE activity_log IN ACCESS EXCLUSIVE MODE;

CREATE TABLE activity_log_partitioned (
    id UUID NOT NULL DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL,
    user_id UUID NOT NULL,
    action TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id UUID NOT NULL,
    metadata_json JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY HASH (workspace_id);

DO $$
DECLARE i INT;
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF activity_log_partitioned '
            'FOR VALUES WITH (MODULUS 64, REMAINDER %s)',
            'activity_log_p' || i, i
        );
    END LOOP;
END$$;

INSERT INTO activity_log_partitioned
    (id, workspace_id, user_id, action, resource_type, resource_id, metadata_json, created_at)
SELECT id, workspace_id, user_id, action, resource_type, resource_id, metadata_json, created_at
FROM activity_log;

DROP TABLE activity_log;
ALTER TABLE activity_log_partitioned RENAME TO activity_log;

ALTER TABLE activity_log
    ADD CONSTRAINT activity_log_pkey PRIMARY KEY (id, workspace_id);
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

CREATE TABLE audit_log_partitioned (
    id UUID NOT NULL DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL,
    actor_id UUID,
    action VARCHAR(128) NOT NULL,
    resource_type VARCHAR(64),
    resource_id UUID,
    ip_address INET,
    user_agent TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY HASH (workspace_id);

DO $$
DECLARE i INT;
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF audit_log_partitioned '
            'FOR VALUES WITH (MODULUS 64, REMAINDER %s)',
            'audit_log_p' || i, i
        );
    END LOOP;
END$$;

INSERT INTO audit_log_partitioned
    (id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata, created_at)
SELECT id, workspace_id, actor_id, action, resource_type, resource_id, ip_address, user_agent, metadata, created_at
FROM audit_log;

DROP TABLE audit_log;
ALTER TABLE audit_log_partitioned RENAME TO audit_log;

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id, workspace_id);
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

-- Detach the global sequence so the upcoming DROP TABLE of the old
-- change_log does not cascade-drop it; we re-attach it to the new
-- column once the partitioned table owns the name.
ALTER SEQUENCE change_log_sequence_seq OWNED BY NONE;

CREATE TABLE change_log_partitioned (
    sequence BIGINT NOT NULL DEFAULT nextval('change_log_sequence_seq'),
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
) PARTITION BY HASH (workspace_id);

DO $$
DECLARE i INT;
BEGIN
    FOR i IN 0..63 LOOP
        EXECUTE format(
            'CREATE TABLE %I PARTITION OF change_log_partitioned '
            'FOR VALUES WITH (MODULUS 64, REMAINDER %s)',
            'change_log_p' || i, i
        );
    END LOOP;
END$$;

INSERT INTO change_log_partitioned
    (sequence, workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata, occurred_at)
SELECT sequence, workspace_id, actor_id, kind, op, resource_id, parent_id, name, metadata, occurred_at
FROM change_log;

DROP TABLE change_log;
ALTER TABLE change_log_partitioned RENAME TO change_log;

-- Re-attach the shared sequence to the new column so it is dropped
-- with the table on a future teardown and so ALTER SEQUENCE ... OWNED
-- BY bookkeeping stays correct.
ALTER SEQUENCE change_log_sequence_seq OWNED BY change_log.sequence;

ALTER TABLE change_log
    ADD CONSTRAINT change_log_pkey PRIMARY KEY (sequence, workspace_id);
ALTER TABLE change_log
    ADD CONSTRAINT change_log_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;
ALTER TABLE change_log
    ADD CONSTRAINT change_log_actor_id_fkey FOREIGN KEY (actor_id) REFERENCES users(id) ON DELETE SET NULL;

CREATE INDEX idx_change_log_workspace_sequence
    ON change_log(workspace_id, sequence);
CREATE INDEX idx_change_log_workspace_occurred_at
    ON change_log(workspace_id, occurred_at DESC);

-- Tenant isolation for change_log (newly added here)
-- --------------------------------------------------
-- change_log was created in migration 029, AFTER the RLS migration 024,
-- so it never received a tenant_isolation policy — it is the only
-- workspace-scoped append-only log without one. Since this migration is
-- already rebuilding the table, we close that defense-in-depth gap so
-- change_log matches activity_log and audit_log. The predicate is
-- identical to migration 024: when the app.workspace_id GUC is unset
-- (migrations and background workers — e.g. the changefeed paths in
-- cmd/worker that deliberately run without the GUC) the
-- app_current_workspace_id() IS NULL branch keeps those paths
-- unrestricted, and request-context connections only ever read/write
-- change_log for their own bound workspace, so the policy is a no-op on
-- the happy path and a backstop against a future query that forgets the
-- workspace_id filter. (The down migration intentionally does NOT
-- recreate this policy: reverting 033 must restore the pre-033 state,
-- in which change_log had no RLS.)
CREATE POLICY tenant_isolation ON change_log
    USING (app_current_workspace_id() IS NULL
           OR workspace_id = app_current_workspace_id())
    WITH CHECK (app_current_workspace_id() IS NULL
                OR workspace_id = app_current_workspace_id());
ALTER TABLE change_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE change_log FORCE ROW LEVEL SECURITY;
