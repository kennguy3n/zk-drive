-- retention_policies express per-folder (or workspace-wide when
-- folder_id IS NULL) rules for how long file versions are retained
-- before they are archived or deleted. The retention worker reads this
-- table and drives both cold-archive and hard-delete passes.
CREATE TABLE retention_policies (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id),
    folder_id UUID REFERENCES folders(id),
    max_versions INT,
    max_age_days INT,
    archive_after_days INT,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unique per (workspace, folder) so POSTs upsert rather than create
-- duplicates. COALESCE maps NULL folder_id to a sentinel UUID because
-- Postgres treats NULL as distinct under plain UNIQUE.
CREATE UNIQUE INDEX idx_retention_policies_scope
    ON retention_policies (workspace_id, COALESCE(folder_id, '00000000-0000-0000-0000-000000000000'::uuid));
