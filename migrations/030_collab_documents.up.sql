-- Collaborative documents (P2 — collab editor backbone).
--
-- ZK Drive's competitive gap analysis ranked "no native collaborative
-- document editor" as the #1 reason customers fall back to Google Drive
-- or OneDrive once they've adopted ZK Drive for file storage. This
-- migration lands the persistence layer for the editor; the Yjs
-- WebSocket provider and TipTap frontend land in follow-up PRs.
--
-- # Privacy boundary architecture
--
-- A document's encryption mode is NOT stored on the document row —
-- it is LIVE-INHERITED from the parent folder's `encryption_mode`
-- column (see migration 018 / internal/folder/folder.go). The
-- folder's mode is effectively immutable once set: the only path
-- that changes it is an explicit admin "migrate folder" action that
-- re-encrypts every object under the folder before flipping the
-- mode. The folder service already rejects cross-mode moves (see
-- internal/folder/service.go:Move ErrEncryptionModeMismatch), so a
-- document's effective privacy boundary cannot silently regress.
--
-- This means we can read `folders.encryption_mode` whenever we need
-- the document's privacy posture, without denormalising it onto the
-- document row and risking drift.
--
-- # Collab capability matrix
--
-- The folder's `encryption_mode` determines the maximum collab
-- feature set the document can use. The user picks a `collab_mode`
-- within that ceiling at create time. The capability resolver
-- (internal/document/capability.go) is the single source of truth
-- for what each (encryption_mode, collab_mode) pair is allowed to
-- do — never duplicate this logic in handlers.
--
--   folder.encryption_mode = 'managed_encrypted'
--     → allowed collab_modes: 'markdown', 'rich', 'rich_presence',
--       'disabled'. Server can decrypt the Y.Doc bytes to fold
--       deltas into snapshots and route awareness through the
--       workspace WebSocket hub.
--
--   folder.encryption_mode = 'strict_zk'
--     → allowed collab_modes: 'markdown', 'disabled'. Server stores
--       opaque ciphertext blobs only; deltas are concatenated rather
--       than merged. Awareness is restricted to "user X is editing"
--       presence pings without cursor or selection payloads.
--
-- # Schema
--
-- documents — the long-lived document row. `y_state` holds the
--   latest server-known Yjs state vector (snapshot) and is updated
--   by the compaction job every COMPACTION_DELTA_COUNT (= 64)
--   appended deltas. Snapshot replaces the deltas it folded in via
--   a single SERIALIZABLE transaction so readers either see the
--   pre-fold (snapshot + deltas) or the post-fold (new snapshot,
--   deltas trimmed) — never a torn state. y_state_vector is the
--   Yjs state vector for incremental delta requests (RFC: y-protocols
--   sync step 1).
--
-- document_deltas — append-only log of Yjs updates between
--   snapshots. The `seq` column is a per-document monotonic
--   bigserial assigned by the database; clients use it as the
--   cursor for catch-up. Deltas older than the latest snapshot's
--   `y_state_seq_floor` are deleted by the compaction job.
--
-- # Indexes
--
-- Hot query paths:
--   1. "list documents in folder X" → idx_documents_folder
--   2. "list documents created by user X" → idx_documents_created_by
--   3. "fetch deltas for document D after seq N" → idx_document_deltas_doc_seq
--      (PRIMARY KEY (document_id, seq) gives this directly)
--   4. "list documents updated since timestamp T for changefeed"
--      → idx_documents_workspace_updated_at

CREATE TABLE documents (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- folder_id is REQUIRED (no root-level documents). The privacy
    -- boundary lives on the folder and a folder-less document would
    -- have no encryption mode to inherit. The "Documents" virtual
    -- folder at the workspace root is a regular folders row created
    -- on workspace provisioning (or lazily on first document create
    -- in the service layer).
    folder_id           UUID NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
    name                TEXT NOT NULL,
    -- collab_mode is the user's choice within the folder's capability.
    -- 'disabled' is a tombstone state for documents whose folder was
    -- moved into a mode that doesn't allow collab; the document
    -- becomes read-only until the user explicitly re-enables it (or
    -- moves it back to an allowing folder).
    collab_mode         TEXT NOT NULL DEFAULT 'markdown',
    -- y_state is the encrypted Yjs snapshot. For managed_encrypted
    -- folders this is wrapped under the workspace DEK and the server
    -- can decrypt to merge. For strict_zk folders this is a
    -- client-side AEAD blob; the server never decrypts.
    y_state             BYTEA NOT NULL DEFAULT '\x',
    -- y_state_vector is the Yjs state vector (Y.encodeStateVector
    -- output) at the snapshot point. Clients use it to request
    -- incremental updates rather than re-downloading the whole
    -- snapshot. Empty for a freshly-created document.
    y_state_vector      BYTEA NOT NULL DEFAULT '\x',
    -- y_state_seq_floor is the highest delta seq folded INTO the
    -- current snapshot. Deltas with seq > y_state_seq_floor are
    -- the "tail" that clients need on top of the snapshot to
    -- reconstruct the current document state.
    y_state_seq_floor   BIGINT NOT NULL DEFAULT 0,
    -- snapshot_version is bumped every time y_state is rewritten.
    -- Clients hold onto a snapshot_version + state_vector and use
    -- both to decide whether their cached snapshot is still valid.
    snapshot_version    BIGINT NOT NULL DEFAULT 0,
    created_by          UUID NOT NULL REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ,

    CONSTRAINT documents_name_nonempty   CHECK (length(name) > 0 AND length(name) <= 512),
    CONSTRAINT documents_collab_mode_known CHECK (collab_mode IN ('markdown','rich','rich_presence','disabled'))
);

CREATE INDEX idx_documents_folder
    ON documents (folder_id, deleted_at)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_documents_created_by
    ON documents (created_by, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX idx_documents_workspace_updated_at
    ON documents (workspace_id, updated_at DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON documents
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );


CREATE TABLE document_deltas (
    -- (document_id, seq) is the natural primary key. We assign seq
    -- via a per-document sequence pattern: the INSERT statement
    -- computes `COALESCE(MAX(seq), 0) + 1 FROM document_deltas
    -- WHERE document_id = $1` inside a SERIALIZABLE transaction.
    -- This is slower than a global BIGSERIAL but keeps the seq
    -- numbers per-document monotonic without leaking sequence
    -- numbers across documents (which would be a side channel
    -- revealing other documents' activity to a curious client).
    document_id         UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    seq                 BIGINT NOT NULL,
    -- payload is an encrypted Yjs update binary. For managed_encrypted
    -- folders the server unwraps + merges into snapshots; for
    -- strict_zk folders it's stored opaquely and forwarded as-is to
    -- subscribed clients via the WebSocket hub.
    payload             BYTEA NOT NULL,
    author_user_id      UUID NOT NULL REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- workspace_id is denormalised here so RLS works without a join
    -- to documents. The service layer ensures it always matches the
    -- parent document's workspace_id (it's set from the same
    -- request context that already established workspace isolation).
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    PRIMARY KEY (document_id, seq),
    CONSTRAINT document_deltas_payload_nonempty CHECK (length(payload) > 0),
    CONSTRAINT document_deltas_payload_bounded  CHECK (length(payload) <= 1048576)
);

CREATE INDEX idx_document_deltas_workspace_created
    ON document_deltas (workspace_id, created_at DESC);

ALTER TABLE document_deltas ENABLE ROW LEVEL SECURITY;
ALTER TABLE document_deltas FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON document_deltas
    USING (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    )
    WITH CHECK (
        app_current_workspace_id() IS NULL
        OR workspace_id = app_current_workspace_id()
    );
