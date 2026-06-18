-- Guided setup wizard completion flag.
--
-- A fresh SME deployment boots with an empty database and no operator
-- knowledge. The frontend drives a first-boot wizard (admin account →
-- storage → optional services → first workspace → first invite); the
-- server exposes GET /api/setup/status so the UI can decide whether to
-- show that wizard and which steps still need attention.
--
-- "Are we set up yet?" is *mostly* derivable from existing state — a
-- workspace existing implies the operator got through the wizard — but
-- the spec requires an explicit, durable completion flag so that:
--   * dismissing the wizard is sticky even on an install that later
--     has all its workspaces deleted, and
--   * the admin can intentionally mark setup done without us guessing
--     from heuristics.
--
-- # Singleton model
--
-- This is global, install-wide state (NOT per-tenant), so the table is
-- deliberately a single-row singleton — the same pattern and tenancy
-- stance as worker_heartbeats (migration 039) and platform_api_keys
-- (migration 036): no workspace_id and therefore not subject to the
-- tenant_isolation RLS policies. The single row is pinned by a
-- BOOLEAN primary key fixed to TRUE via a CHECK constraint, so a
-- second INSERT can only ever conflict on the primary key — there is
-- physically no way to grow a second row.
CREATE TABLE setup_state (
    id           BOOLEAN PRIMARY KEY DEFAULT TRUE,
    completed    BOOLEAN NOT NULL DEFAULT FALSE,
    completed_at TIMESTAMPTZ,
    CONSTRAINT setup_state_singleton CHECK (id)
);

-- Seed the singleton row so readers never have to special-case "no
-- row yet" — a fresh install reads completed = FALSE. ON CONFLICT DO
-- NOTHING keeps the migration idempotent if it is ever re-applied.
INSERT INTO setup_state (id, completed)
VALUES (TRUE, FALSE)
ON CONFLICT (id) DO NOTHING;
