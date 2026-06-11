-- Worker liveness heartbeats (WS8 Observability & NoOps).
--
-- The API server has no NATS-level visibility into whether the
-- background worker fleet is actually consuming jobs: a worker can be
-- network-partitioned from JetStream, wedged on a slow ClamAV, or
-- simply not deployed at all, and the only previous symptom was jobs
-- silently piling up. This table gives the server a cheap, pull-based
-- liveness signal it can render on the admin health dashboard
-- (GET /api/admin/health-dashboard → "worker" subsystem) without
-- coupling the two processes over the message bus.
--
-- # Model
--
-- One row per (worker_type, instance_id). A single worker process
-- upserts one row for each logical worker type it actively runs
-- (preview, scan, index, archive, retention, classify); running N
-- worker replicas therefore yields N rows per type. The recorder in
-- internal/heartbeat refreshes last_seen_at on a fixed cadence
-- (heartbeat.DefaultInterval), and the dashboard reader flags a type
-- as stale when the newest instance's last_seen_at is older than a
-- small multiple of that cadence.
--
-- # status
--
-- Free-form TEXT (no CHECK / ENUM, matching the provisioned_by /
-- tier conventions elsewhere in the schema so adding a status is a
-- code-only change). Current producers emit:
--   'ok'       — the worker type is running normally.
--   'degraded' — running but with a dependency disabled (e.g. virus
--                scanning auto-disabled because ClamAV is unreachable;
--                see the worker self-healing path in cmd/worker).
-- The dashboard maps 'degraded' to yellow and a stale/absent type to
-- red.
--
-- # detail
--
-- JSONB bag of worker-type-specific context (e.g. {"virus_scanning":
-- false} when scan is degraded). Kept as JSONB so producers can
-- evolve the shape without a migration; the dashboard treats it as
-- opaque pass-through to the operator UI.
--
-- # Tenancy
--
-- There is NO workspace_id: the worker fleet is global infrastructure
-- shared by every tenant, so this table is deliberately NOT subject to
-- the tenant_isolation RLS policies from migration 024 — exactly like
-- platform_api_keys (migration 036). It is only ever read through the
-- admin health dashboard, which is already gated by the workspace
-- admin role at the HTTP layer.
CREATE TABLE worker_heartbeats (
    worker_type  TEXT NOT NULL,
    instance_id  TEXT NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status       TEXT NOT NULL DEFAULT 'ok',
    detail       JSONB NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (worker_type, instance_id)
);

-- The dashboard reader's only query groups by worker_type and reads
-- the newest last_seen_at per group. The PRIMARY KEY's backing btree
-- on (worker_type, instance_id) already serves the per-type grouping
-- as a leading-column prefix scan, so no extra index is needed at the
-- fleet sizes this table holds (one row per worker type per replica —
-- tens of rows, not millions).
