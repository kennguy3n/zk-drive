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
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ip_allowlist_ws ON workspace_ip_allowlist(workspace_id);

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
