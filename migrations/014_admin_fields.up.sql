-- Admin dashboard support: track last login for each user and allow
-- admins to soft-deactivate users without deleting rows so audit history
-- remains intact.
ALTER TABLE users
    ADD COLUMN last_login_at TIMESTAMPTZ,
    ADD COLUMN deactivated_at TIMESTAMPTZ;

CREATE INDEX idx_users_workspace_active ON users(workspace_id)
    WHERE deactivated_at IS NULL;
