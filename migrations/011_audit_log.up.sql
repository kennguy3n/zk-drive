-- audit_log records security-relevant events distinct from the
-- user-facing activity_log: login/logout, password change, SSO link,
-- permission grant/revoke, admin user management, workspace settings.
CREATE TABLE audit_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    actor_id UUID,
    action VARCHAR(128) NOT NULL,
    resource_type VARCHAR(64),
    resource_id UUID,
    ip_address INET,
    user_agent TEXT,
    metadata JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_log_workspace ON audit_log(workspace_id, created_at DESC);
