-- preview status on file versions (WS8 auto-healing).
--
-- preview_status tracks the async preview-render lifecycle for each
-- uploaded version so a permanently-undecodable payload (or a
-- persistent renderer fault) is recorded as terminal instead of being
-- Nak-looped by the worker until JetStream's QueueMaxDeliver cap
-- finally drops it:
--   pending      — newly uploaded, preview job enqueued but not yet
--                  rendered (default; also the state while retries are
--                  still in flight)
--   done         — a preview was rendered and a file_previews row
--                  exists. Set on the worker's success path so an
--                  operator can distinguish "no preview yet" from
--                  "rendered" without joining file_previews.
--   unsupported  — the version's MIME type has no preview renderer
--                  (e.g. an opaque binary). Terminal: the worker acks
--                  the job as a skip and never retries.
--   failed       — preview generation failed on PreviewMaxAttempts
--                  consecutive deliveries. Terminal: the worker marks
--                  this and acks (skips) the job so the preview subject
--                  is not wedged forever by one poison payload.
--
-- preview_detail captures the last error / skip reason so admins can
-- audit failures without rummaging through worker logs, mirroring the
-- scan_status / scan_detail pair added in migration 008.
--
-- The column is added to the partitioned file_versions table; ALTER
-- TABLE ADD COLUMN with a constant default is metadata-only on
-- Postgres 11+ and propagates to every existing and future partition,
-- so this is safe to run online on a large fleet.

ALTER TABLE file_versions
    ADD COLUMN preview_status TEXT NOT NULL DEFAULT 'pending'
        CHECK (preview_status IN ('pending', 'done', 'unsupported', 'failed')),
    ADD COLUMN preview_detail TEXT;

-- Partial index so the worker / dashboard can cheaply enumerate the
-- (expected-rare) terminal-failure rows for re-drive or alerting
-- without scanning the overwhelmingly 'pending'/'done' majority.
CREATE INDEX idx_file_versions_preview_failed
    ON file_versions(preview_status)
    WHERE preview_status = 'failed';
