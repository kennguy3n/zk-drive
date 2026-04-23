CREATE TABLE folders (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    parent_folder_id UUID REFERENCES folders(id),
    name TEXT NOT NULL,
    path TEXT NOT NULL DEFAULT '/',
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_folders_unique_name
    ON folders(workspace_id, parent_folder_id, name)
    WHERE deleted_at IS NULL AND parent_folder_id IS NOT NULL;

CREATE UNIQUE INDEX idx_folders_unique_root_name
    ON folders(workspace_id, name)
    WHERE deleted_at IS NULL AND parent_folder_id IS NULL;

CREATE INDEX idx_folders_workspace ON folders(workspace_id);
CREATE INDEX idx_folders_parent ON folders(workspace_id, parent_folder_id);
