-- Phase 2: share links and guest invites.
--
-- share_links are independent of the user permission model
-- (ARCHITECTURE.md §7.3): holding the token grants access. A link may
-- carry a bcrypt-hashed password, an expiry timestamp, and / or a
-- max_downloads cap. download_count is incremented on every successful
-- resolution so the server can enforce the cap.
--
-- guest_invites record an invitation for an external user to
-- collaborate on a folder. Creating an invite also creates a
-- `permissions` row (grantee_type='guest') with the same expires_at;
-- the permission_id back-reference lets the retention worker revoke
-- both together when the invite lapses.

CREATE TABLE share_links (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    resource_type TEXT NOT NULL CHECK (resource_type IN ('folder', 'file')),
    resource_id UUID NOT NULL,
    token TEXT NOT NULL UNIQUE,
    password_hash TEXT,
    expires_at TIMESTAMPTZ,
    max_downloads INTEGER CHECK (max_downloads IS NULL OR max_downloads > 0),
    download_count INTEGER NOT NULL DEFAULT 0,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_share_links_workspace ON share_links(workspace_id);
CREATE INDEX idx_share_links_resource ON share_links(workspace_id, resource_type, resource_id);

CREATE TABLE guest_invites (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    email TEXT NOT NULL,
    folder_id UUID NOT NULL REFERENCES folders(id),
    role TEXT NOT NULL CHECK (role IN ('viewer', 'editor', 'admin')),
    expires_at TIMESTAMPTZ,
    accepted_at TIMESTAMPTZ,
    permission_id UUID NOT NULL REFERENCES permissions(id),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_guest_invites_workspace ON guest_invites(workspace_id);
CREATE INDEX idx_guest_invites_folder ON guest_invites(workspace_id, folder_id);
CREATE INDEX idx_guest_invites_email ON guest_invites(workspace_id, lower(email));
