-- Phase 2: file previews.
--
-- Each row represents a server-rendered preview (thumbnail) for a
-- specific file version. The preview bytes live in zk-object-fabric
-- alongside the source object under the dedicated key
--   {workspace_id}/{file_id}/{version_id}/preview.png
-- so retention / garbage collection can remove them by prefix when the
-- underlying version is deleted.
--
-- One preview per (file_id, version_id) pair. When re-generation is
-- needed the worker UPDATEs the row in place; ON CONFLICT (file_id,
-- version_id) DO UPDATE is the insert path used by
-- internal/preview/repository.go.

CREATE TABLE file_previews (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    file_id UUID NOT NULL REFERENCES files(id),
    version_id UUID NOT NULL REFERENCES file_versions(id),
    object_key TEXT NOT NULL,
    mime_type TEXT NOT NULL DEFAULT 'image/png',
    size_bytes BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(file_id, version_id)
);

CREATE INDEX idx_file_previews_file ON file_previews(file_id);
