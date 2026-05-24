-- Audit-log cold archival.
--
-- The audit_log table records every security-relevant event (login,
-- permission grant/revoke, admin user management, retention-policy
-- edit, MFA lifecycle, guest-invite email delivery). It is INSERT-only
-- and indexed on (workspace_id, created_at DESC) — perfect for hot
-- queries from the admin console, but unbounded. A workspace that has
-- run for two years with active users carries hundreds of thousands
-- of rows; the index alone grows linearly. For SOC2 Type II, HIPAA,
-- and GDPR, operators need:
--
--   1. A documented retention policy on the hot tier (90 days is
--      typical and is the AUDIT_LOG_RETENTION_DAYS default).
--   2. A queryable cold tier for incident investigation across the
--      full historical period (typically 7 years for SOC2, 6 years
--      for HIPAA, indefinite for legal-hold).
--   3. A documented restore workflow so a regulator's "produce all
--      admin actions in workspace X between two dates" request can
--      be satisfied within the SLA.
--
-- The cold tier is JSONL.gz objects in the same zk-object-fabric S3
-- bucket the application already uses, under a configurable prefix
-- (default `audit-archive/`). Each archive object covers a single
-- (workspace_id, year-month) batch and is named with a UUID suffix:
--
--   {prefix}{workspace_id}/{year-month}/{run_id}.jsonl.gz
--
-- The UUID suffix is the load-bearing part of the design: it makes
-- a re-run of the archiver against the same (workspace, year-month)
-- range produce a DIFFERENT key on every invocation, so a crash
-- between the S3 upload and the audit_log DELETE leaves the rows in
-- place and the next run simply writes a fresh object next to the
-- partial one. No row is lost; the worst case is a duplicate object
-- in the cold tier (and the restore tool de-duplicates by id on
-- read).
--
-- audit_log_archive_runs records one row per (run_id, workspace_id,
-- year-month) batch. error_message is non-NULL when the batch failed
-- mid-flight (and the corresponding rows are still in audit_log,
-- ready for the next sweep). The table is append-only on the happy
-- path; the restore tool reads it to enumerate which S3 objects
-- exist for a workspace's history.
--
-- RLS: the table joins the standard tenant_isolation policy (so an
-- in-workspace admin can `SELECT * FROM audit_log_archive_runs` and
-- see only their own workspace's archive history), with the same
-- app_current_workspace_id() IS NULL bypass the archiver itself
-- uses to enumerate every workspace.
CREATE TABLE audit_log_archive_runs (
    id                  UUID        PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- run_id groups every per-workspace, per-month batch that one
    -- archiver invocation processed. A single nightly run typically
    -- inserts O(active_workspaces) rows sharing this id.
    run_id              UUID        NOT NULL,
    workspace_id        UUID        NOT NULL REFERENCES workspaces(id),
    -- cutoff_time is the inclusive upper bound on audit_log.created_at
    -- for rows captured in this batch. The archiver computes cutoff
    -- once per run (now() - retention_days * INTERVAL '1 day') and
    -- carries it through every per-workspace batch so concurrent
    -- runs against overlapping retention windows produce comparable
    -- output.
    cutoff_time         TIMESTAMPTZ NOT NULL,
    -- year_month groups the batched rows by their created_at month
    -- (UTC). One archive object per (workspace, month) means the
    -- cold tier's key layout matches the natural query pattern
    -- ("find all admin actions in March 2024 for workspace X")
    -- without a giant per-workspace shard.
    year_month          CHAR(7)     NOT NULL, -- e.g. "2024-03"
    archive_object_key  TEXT        NOT NULL,
    -- rows_archived is the count of audit_log rows uploaded in this
    -- batch (equal to the number of JSONL records in the S3 object
    -- on the happy path). Useful for the operator dashboard and for
    -- detecting silent regressions: if rows_archived drops to zero
    -- while audit_log row count keeps growing, something is broken.
    rows_archived       INTEGER     NOT NULL,
    -- bytes_uploaded is the UNCOMPRESSED JSONL payload size in
    -- bytes, NOT the on-the-wire size of the gzipped object that
    -- landed in S3. The archiver records the source-of-truth
    -- byte count (the input that produced the gzip stream) so
    -- the operator dashboard can plot a meaningful rows/byte
    -- ratio independent of gzip's compression behaviour on each
    -- workspace's audit traffic. The matching Prometheus metric
    -- zkdrive_audit_archive_bytes_total carries the same
    -- "uncompressed JSONL bytes" semantic; the two must agree.
    -- Operators who want S3 storage cost should multiply by the
    -- empirically-observed gzip ratio for their workload (≈10x
    -- compression on real audit traffic).
    bytes_uploaded      BIGINT      NOT NULL,
    started_at          TIMESTAMPTZ NOT NULL,
    completed_at        TIMESTAMPTZ NOT NULL,
    -- error_message is the load-bearing column for partial-failure
    -- visibility. On success it is NULL; on failure it carries the
    -- error string from the upload or DELETE step and the matching
    -- audit_log rows are still in the hot tier. The next archiver
    -- run will re-attempt the batch (idempotent — fresh run_id,
    -- fresh archive_object_key).
    error_message       TEXT
);

CREATE INDEX idx_audit_log_archive_runs_workspace
    ON audit_log_archive_runs(workspace_id, completed_at DESC);

CREATE INDEX idx_audit_log_archive_runs_year_month
    ON audit_log_archive_runs(workspace_id, year_month);

-- Failed runs are the operator's debugging surface. A partial index
-- on error_message IS NOT NULL keeps the operator dashboard query
-- ("show me every audit archive failure in the last 24h") cheap even
-- as the success rows pile up.
CREATE INDEX idx_audit_log_archive_runs_failures
    ON audit_log_archive_runs(completed_at DESC)
    WHERE error_message IS NOT NULL;

-- RLS: join the standard tenant_isolation pattern (see migration 024).
-- An in-workspace admin querying the table sees only their own
-- workspace's archive history; the archiver runs without setting
-- app.workspace_id and gets the bypass branch.
CREATE POLICY tenant_isolation ON audit_log_archive_runs
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
ALTER TABLE audit_log_archive_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log_archive_runs FORCE ROW LEVEL SECURITY;
