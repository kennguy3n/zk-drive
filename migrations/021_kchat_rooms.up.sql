-- KChat room → ZK Drive folder mapping (Phase 4, task 8).
--
-- Each KChat room that needs a backing storage area maps 1:1 to a
-- ZK Drive folder. The folder lives in the regular folders tree so
-- every existing endpoint (listing, permissions, search) works
-- transparently against it; this side table just records the
-- KChat-side identifier and the creator for audit purposes.
--
-- The (workspace_id, kchat_room_id) uniqueness constraint enforces
-- the 1:1 mapping per workspace and lets the upsert path detect
-- "already mapped" without a separate read.
CREATE TABLE kchat_room_folders (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    kchat_room_id TEXT NOT NULL,
    folder_id UUID NOT NULL REFERENCES folders(id),
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, kchat_room_id)
);

CREATE INDEX idx_kchat_room_folders_workspace ON kchat_room_folders(workspace_id);
