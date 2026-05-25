-- change_log records every state-mutating operation on a workspace's
-- files / folders / permissions so desktop sync clients (and future
-- mobile sync clients) can replay missed events on reconnect.
--
-- The semantics differ from activity_log in two important ways:
--
--   1. activity_log is fire-and-forget telemetry, written by an async
--      worker that drops entries when its in-memory buffer overflows.
--      change_log is written synchronously on the request path so
--      every successful mutation is durable before the HTTP response.
--   2. activity_log records every action (including reads — downloads,
--      preview generation). change_log records only state mutations
--      that desktop sync clients need to react to.
--
-- Cursor model: BIGSERIAL `sequence` is the global monotonic ordering.
-- Sync clients store the highest `sequence` they have seen and
-- subsequently fetch `WHERE workspace_id = $1 AND sequence > $cursor
-- ORDER BY sequence LIMIT $N`. Per-workspace monotonicity is
-- preserved because INSERT order within a single workspace is
-- preserved by BIGSERIAL (rows for the same workspace can never
-- arrive out of order — Postgres assigns sequence values during
-- INSERT in a single CPU-ordered step).
--
-- A global sequence rather than per-workspace was chosen for two
-- reasons:
--   - Simplicity: BIGSERIAL is a single sequence object with no
--     per-workspace bookkeeping table.
--   - Auditability: a single monotonic counter makes it easy to
--     spot data loss across the whole platform — if the highest
--     sequence number doesn't match the row count, something was
--     deleted out of band.
--
-- The bigint range (9.2 * 10^18) is comfortably bigger than any
-- realistic cumulative platform mutation count for the next 1000
-- years at 1 billion mutations/sec.

CREATE TABLE change_log (
    sequence BIGSERIAL PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    kind TEXT NOT NULL CHECK (kind IN ('file', 'folder', 'permission')),
    op TEXT NOT NULL CHECK (op IN ('create', 'update', 'rename', 'move', 'delete')),
    resource_id UUID NOT NULL,
    parent_id UUID,
    name TEXT,
    metadata JSONB,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The cursor pagination query is exclusively
-- `WHERE workspace_id = $1 AND sequence > $cursor ORDER BY sequence LIMIT N`.
-- An ordered composite index lets Postgres do an index-only range
-- scan and serve every page from the same B-tree path.
CREATE INDEX idx_change_log_workspace_sequence
    ON change_log(workspace_id, sequence);

-- Bookkeeping queries (e.g. "what did user X do last week") use
-- (workspace_id, occurred_at). Lower priority than the cursor scan
-- but cheap to maintain.
CREATE INDEX idx_change_log_workspace_occurred_at
    ON change_log(workspace_id, occurred_at DESC);
