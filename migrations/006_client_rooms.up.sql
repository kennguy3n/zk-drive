-- Phase 2: client rooms.
--
-- A client room is a "folder + share link" bundle dedicated to an
-- external client or partner. The folder lives inside the regular
-- folders tree (so existing permission / listing / move code works
-- unmodified) and the share link provides the external access URL.
-- Storing the room as its own row lets the UI list them separately
-- from ordinary folders and lets us attach room-level metadata
-- (dropbox-only mode, expiry, etc.) without polluting the folders
-- table.
--
-- dropbox_enabled flags rooms where external visitors may only upload,
-- not download — enforced by the share-link handler; stored here as
-- the authoritative room policy.

CREATE TABLE client_rooms (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    name TEXT NOT NULL,
    folder_id UUID NOT NULL REFERENCES folders(id),
    share_link_id UUID NOT NULL REFERENCES share_links(id),
    dropbox_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at TIMESTAMPTZ,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_client_rooms_workspace ON client_rooms(workspace_id);
CREATE UNIQUE INDEX idx_client_rooms_folder ON client_rooms(folder_id);
CREATE UNIQUE INDEX idx_client_rooms_share_link ON client_rooms(share_link_id);
