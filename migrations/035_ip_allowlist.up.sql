-- Per-workspace IP allowlisting (conditional access).
--
-- Regulated SME admins can restrict access to their workspace to a
-- set of trusted networks (office ranges, VPN egress, etc.). When
-- workspaces.ip_allowlist_enabled is TRUE, the api/middleware
-- IPAllowlist guard rejects any authenticated request whose
-- resolved client IP is not contained by one of the workspace's
-- CIDR rules. When FALSE (the default) the guard is a no-op and
-- access is unrestricted.
--
-- The allowlist is a positive list of public CIDRs. The
-- application layer (internal/workspace.IPAllowService) refuses to
-- store RFC1918 / loopback / link-local ranges: allowlisting a
-- private range on a public multi-tenant SaaS would either match
-- nothing (the gateway never sees a private source) or, worse,
-- match a co-tenant behind the same NAT. The CIDR type here only
-- guarantees a well-formed network; the public-range check lives in
-- Go alongside the rest of the validation (cap of 50 rules/ws).
--
-- RLS: workspace_ip_allowlist carries an authoritative workspace_id
-- column, so it is a "direct tenant table" in the sense of
-- migration 024_row_level_security.up.sql. We add the same
-- tenant_isolation policy (USING + WITH CHECK keyed on
-- app_current_workspace_id()) and FORCE ROW LEVEL SECURITY so the
-- app's non-superuser role is also subject to it. The IP guard runs
-- inside the authenticated request lifecycle with app.workspace_id
-- already bound by the pgxpool PrepareConn hook, so the policy
-- resolves to the caller's own rows; the bypass branch
-- (app_current_workspace_id() IS NULL) keeps migrations and
-- background workers working unchanged.

CREATE TABLE workspace_ip_allowlist (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID       NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    cidr        CIDR        NOT NULL,
    label       TEXT        NOT NULL DEFAULT '',
    created_by  UUID        NOT NULL REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- A CIDR is a positive admission rule; storing the same range
    -- twice (even under different labels) cannot widen access and
    -- only burns one of the 50 rule slots. Enforce uniqueness in the
    -- schema so AddRule's cap can't be silently wasted on duplicates
    -- and so two concurrent adds of the same range can't both land.
    -- cidr is canonicalised (host bits zeroed) by the Go layer before
    -- insert, so "203.0.113.5/24" and "203.0.113.0/24" collide here.
    CONSTRAINT uq_ip_allowlist_ws_cidr UNIQUE (workspace_id, cidr)
);

-- No standalone index on (workspace_id): the uq_ip_allowlist_ws_cidr
-- UNIQUE constraint above is backed by a btree on (workspace_id, cidr)
-- whose leftmost column already serves every per-workspace lookup
-- (ListRules ordered scan, the cap COUNT, the CheckAccess fetch). A
-- separate single-column index would be redundant write amplification.

-- tenant_isolation mirrors the direct-table policy from migration
-- 024. Identical predicate so the cross-tenant guarantee is a
-- single source of truth across every tenant table.
CREATE POLICY tenant_isolation ON workspace_ip_allowlist
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
ALTER TABLE workspace_ip_allowlist ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_ip_allowlist FORCE ROW LEVEL SECURITY;

-- Per-workspace master switch. Default FALSE so existing workspaces
-- are unaffected until an admin explicitly opts in via
-- PATCH /api/admin/ip-allowlist/policy.
ALTER TABLE workspaces
    ADD COLUMN ip_allowlist_enabled BOOLEAN NOT NULL DEFAULT false;
