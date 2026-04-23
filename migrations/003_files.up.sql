CREATE TABLE files (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    folder_id UUID NOT NULL REFERENCES folders(id),
    name TEXT NOT NULL,
    current_version_id UUID,
    size_bytes BIGINT NOT NULL DEFAULT 0,
    mime_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE UNIQUE INDEX idx_files_unique_name
    ON files(workspace_id, folder_id, name)
    WHERE deleted_at IS NULL;

CREATE TABLE file_versions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    file_id UUID NOT NULL REFERENCES files(id),
    version_number INT NOT NULL DEFAULT 1,
    object_key TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    checksum TEXT NOT NULL,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(file_id, version_number)
);

ALTER TABLE files ADD CONSTRAINT fk_files_current_version
    FOREIGN KEY (current_version_id) REFERENCES file_versions(id);

CREATE INDEX idx_files_workspace ON files(workspace_id);
CREATE INDEX idx_files_folder ON files(workspace_id, folder_id);
