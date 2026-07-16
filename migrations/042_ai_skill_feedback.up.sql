-- Stores user feedback (thumbs up/down) on AI editor skill output.
-- Used for quality monitoring of local LLM models — operators can
-- aggregate ratings by skill, model, and workspace to track AI quality
-- over time and across model upgrades.
--
-- # Schema
--
-- id is a surrogate PK (UUIDv7 for time-ordering). workspace_id +
-- document_id scope the feedback to the document that produced it.
-- user_id is the editor who rated the output (ON DELETE SET NULL —
-- the feedback survives user deletion, only actor attribution is
-- cleared, matching the audit convention used in workspace_features
-- migration 041). skill_id is the editor skill that produced the
-- output (e.g. "improve_writing", "summarize"). rating is 'up' or
-- 'down'. model records which LLM model was used, so operators can
-- compare quality across model upgrades. created_at is set by the
-- application on insert.
--
-- # RLS
--
-- Uses the same tenant_isolation pattern as the rest of the schema
-- (migration 024).
CREATE TABLE ai_skill_feedback (
    id           UUID        NOT NULL PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    document_id  UUID        NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    user_id      UUID        REFERENCES users(id) ON DELETE SET NULL,
    skill_id     TEXT        NOT NULL CHECK (skill_id <> ''),
    rating       TEXT        NOT NULL CHECK (rating IN ('up', 'down')),
    model        TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Index for the most common query: aggregate ratings by workspace + skill.
CREATE INDEX ai_skill_feedback_workspace_skill_idx
    ON ai_skill_feedback (workspace_id, skill_id, created_at DESC);

ALTER TABLE ai_skill_feedback ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_skill_feedback FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON ai_skill_feedback
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
