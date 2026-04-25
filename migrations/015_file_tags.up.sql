-- File tags: lightweight join table allowing users to attach freeform
-- text labels to files. Tags are workspace-scoped (so search and admin
-- views can filter without cross-tenant leakage) and unique per file
-- so duplicate (file, tag) pairs are rejected at the DB layer rather
-- than by the application.
CREATE TABLE file_tags (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    file_id UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    tag TEXT NOT NULL,
    created_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_file_tags_unique
    ON file_tags(file_id, tag);

CREATE INDEX idx_file_tags_workspace_tag
    ON file_tags(workspace_id, tag);
