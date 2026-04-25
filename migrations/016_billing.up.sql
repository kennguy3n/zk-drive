-- Billing scaffolding: each workspace has at most one workspace_plans
-- row identifying its tier and per-tier override limits. NULL limits
-- mean "use the default for this tier" so admins can set workspace-
-- specific overrides without rewriting the tier table.
CREATE TABLE workspace_plans (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) UNIQUE,
    tier TEXT NOT NULL DEFAULT 'free',
    max_storage_bytes BIGINT,
    max_users INT,
    max_bandwidth_bytes_monthly BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- usage_events is a thin append-only ledger. We don't store cumulative
-- counters because (a) recovery from a bad write is hard with a
-- counter and (b) Postgres aggregates over (workspace, type, time)
-- are cheap with the supporting index. event_type values are kept as
-- TEXT (not enum) so we can add new types without a migration.
CREATE TABLE usage_events (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    event_type TEXT NOT NULL,
    bytes BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_usage_events_lookup
    ON usage_events(workspace_id, event_type, created_at);
