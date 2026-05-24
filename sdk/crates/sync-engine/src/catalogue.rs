//! Local SQLite catalogue tracking sync state for every locally-mirrored
//! file. The schema is private to this crate; downstream callers go
//! through the [`Catalogue`] API.
//!
//! Concurrency: every method opens a fresh transaction so the
//! catalogue is safe to call from multiple tasks; the schema itself
//! is single-writer and uses `journal_mode=WAL` so readers don't
//! block writers.

use std::path::{Path, PathBuf};

use rusqlite::{params, OptionalExtension};
use serde::{Deserialize, Serialize};
use tracing::warn;
use uuid::Uuid;

use crate::Result;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SyncStatus {
    /// Local copy matches the recorded remote version.
    UpToDate,
    /// Local content changed and the file still exists on disk; the
    /// upload flow should push the new bytes.
    LocalDirty,
    /// Local file was deleted (or otherwise no longer exists at
    /// `local_path`); the upload flow should push a tombstone for the
    /// remote object. Kept distinct from [`SyncStatus::LocalDirty`] so
    /// the upload flow doesn't have to stat the path again to
    /// distinguish 'send bytes' from 'send tombstone'.
    LocalDeleted,
    /// Remote change waiting to be downloaded.
    RemoteDirty,
    /// Both sides changed since last sync; the [`crate::conflict::ConflictPolicy`]
    /// decides the resolution.
    Conflict,
    /// File is being uploaded / downloaded right now.
    InFlight,
    /// File was synced earlier but the local copy was evicted under
    /// the offline cache's LRU budget.
    Evicted,
}

impl SyncStatus {
    fn as_str(self) -> &'static str {
        match self {
            SyncStatus::UpToDate => "up_to_date",
            SyncStatus::LocalDirty => "local_dirty",
            SyncStatus::LocalDeleted => "local_deleted",
            SyncStatus::RemoteDirty => "remote_dirty",
            SyncStatus::Conflict => "conflict",
            SyncStatus::InFlight => "in_flight",
            SyncStatus::Evicted => "evicted",
        }
    }

    /// Returns the catalogue status that should be persisted when
    /// the local file is written (created or modified). A pending
    /// remote change (or an in-flight transfer) escalates to
    /// [`SyncStatus::Conflict`] so we don't clobber the remote side's
    /// intent. A row that was previously [`SyncStatus::LocalDeleted`]
    /// is resurrected back to [`SyncStatus::LocalDirty`] because the
    /// path now has content again.
    ///
    /// Truth table (current -> next):
    ///
    /// | current       | next                                           |
    /// |---------------|------------------------------------------------|
    /// | UpToDate      | LocalDirty                                     |
    /// | LocalDirty    | LocalDirty                                     |
    /// | LocalDeleted  | LocalDirty (file resurrected at this path)     |
    /// | RemoteDirty   | Conflict (remote pending + local upsert)       |
    /// | Conflict      | Conflict                                       |
    /// | InFlight      | Conflict (transfer in progress + local upsert) |
    /// | Evicted       | LocalDirty (user re-created an evicted file)   |
    pub fn next_on_local_upsert(self) -> Self {
        match self {
            SyncStatus::UpToDate
            | SyncStatus::Evicted
            | SyncStatus::LocalDirty
            | SyncStatus::LocalDeleted => SyncStatus::LocalDirty,
            SyncStatus::RemoteDirty | SyncStatus::Conflict | SyncStatus::InFlight => {
                SyncStatus::Conflict
            }
        }
    }

    /// Returns the catalogue status that should be persisted when
    /// the local file disappears (removed, or moved away from this
    /// `local_path`). Distinct from [`Self::next_on_local_upsert`]
    /// because the upload flow has to push a *tombstone* rather than
    /// content for these rows. A pending remote change escalates to
    /// [`SyncStatus::Conflict`].
    ///
    /// Truth table (current -> next):
    ///
    /// | current       | next                                           |
    /// |---------------|------------------------------------------------|
    /// | UpToDate      | LocalDeleted                                   |
    /// | LocalDirty    | LocalDeleted (queued upload made obsolete)     |
    /// | LocalDeleted  | LocalDeleted                                   |
    /// | Evicted       | LocalDeleted (server still has the row)        |
    /// | RemoteDirty   | Conflict (remote pending + local delete)       |
    /// | Conflict      | Conflict                                       |
    /// | InFlight      | Conflict (transfer in progress + local delete) |
    pub fn next_on_local_delete(self) -> Self {
        match self {
            SyncStatus::UpToDate
            | SyncStatus::LocalDirty
            | SyncStatus::LocalDeleted
            | SyncStatus::Evicted => SyncStatus::LocalDeleted,
            SyncStatus::RemoteDirty | SyncStatus::Conflict | SyncStatus::InFlight => {
                SyncStatus::Conflict
            }
        }
    }

    /// Mirror of [`Self::next_on_local_upsert`] / [`Self::next_on_local_delete`]
    /// for remote-side changes (catch-up page or live WebSocket frame).
    ///
    /// Truth table (current -> next):
    ///
    /// | current       | next                                           |
    /// |---------------|------------------------------------------------|
    /// | UpToDate      | RemoteDirty                                    |
    /// | RemoteDirty   | RemoteDirty                                    |
    /// | Evicted       | RemoteDirty                                    |
    /// | LocalDirty    | Conflict (local pending + remote change)       |
    /// | LocalDeleted  | Conflict (local tombstone + remote change)     |
    /// | Conflict      | Conflict                                       |
    /// | InFlight      | Conflict (transfer in progress + remote chg)   |
    pub fn next_on_remote_change(self) -> Self {
        match self {
            SyncStatus::UpToDate | SyncStatus::Evicted => SyncStatus::RemoteDirty,
            SyncStatus::RemoteDirty => SyncStatus::RemoteDirty,
            SyncStatus::LocalDirty
            | SyncStatus::LocalDeleted
            | SyncStatus::Conflict
            | SyncStatus::InFlight => SyncStatus::Conflict,
        }
    }

    /// Maps a persisted status string back to a [`SyncStatus`].
    ///
    /// Unknown / unrecognised strings degrade to `UpToDate` so the
    /// catalogue stays openable across SDK upgrades that may have
    /// introduced new status variants, but a `tracing::warn` is
    /// emitted so the operator can investigate. A silent fallthrough
    /// here would mask sync-state corruption.
    fn parse(s: &str) -> Self {
        match s {
            "up_to_date" => SyncStatus::UpToDate,
            "local_dirty" => SyncStatus::LocalDirty,
            "local_deleted" => SyncStatus::LocalDeleted,
            "remote_dirty" => SyncStatus::RemoteDirty,
            "conflict" => SyncStatus::Conflict,
            "in_flight" => SyncStatus::InFlight,
            "evicted" => SyncStatus::Evicted,
            other => {
                warn!(
                    status = other,
                    "unknown SyncStatus persisted in catalogue; falling back to UpToDate"
                );
                SyncStatus::UpToDate
            }
        }
    }
}

/// One row in the local catalogue.
#[derive(Debug, Clone)]
pub struct FileRecord {
    pub remote_file_id: Uuid,
    pub remote_version_id: Uuid,
    pub local_path: PathBuf,
    pub size_bytes: u64,
    /// BLAKE3-256 of the local file's plaintext contents.
    pub content_hash: [u8; 32],
    pub status: SyncStatus,
    /// True if the file is explicitly pinned for offline access
    /// (never evicted from the local cache).
    pub pinned: bool,
    pub updated_at: chrono::DateTime<chrono::Utc>,
}

/// SQLite-backed catalogue.
///
/// A catalogue is *bound* to exactly one `workspace_id` at
/// [`Catalogue::open`] time. Re-opening the same path for a different
/// workspace is rejected with [`crate::SyncError::Other`] so a stray
/// CLI invocation can't accidentally mix rows from two workspaces
/// (the `files` table is keyed by `remote_file_id`/`local_path` only;
/// mixing workspaces would produce hash / state-machine corruption
/// on the next reconciliation). The CLI's status handler can
/// therefore call [`Catalogue::list_all`] knowing the count is
/// already correctly workspace-scoped.
pub struct Catalogue {
    conn: rusqlite::Connection,
    workspace_id: Uuid,
}

impl Catalogue {
    /// Open or create a catalogue at `path`, bound to `workspace_id`.
    /// The schema is applied idempotently on every open so upgrades
    /// that only ever add tables / columns are safe. If the on-disk
    /// catalogue was previously bound to a *different* workspace,
    /// this returns [`crate::SyncError::Other`] -- the caller is
    /// expected to either pick a different catalogue path or migrate
    /// the existing data out-of-band; we never silently overwrite the
    /// stored binding.
    pub fn open(path: impl AsRef<Path>, workspace_id: Uuid) -> Result<Self> {
        let conn = rusqlite::Connection::open(path)?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        conn.execute_batch(SCHEMA)?;
        let want = workspace_id.to_string();
        let stored: Option<String> = conn
            .query_row(
                "SELECT value FROM catalogue_meta WHERE key = 'workspace_id'",
                [],
                |r| r.get(0),
            )
            .optional()?;
        match stored {
            Some(existing) if existing == want => {}
            Some(existing) => {
                return Err(crate::SyncError::Other(format!(
                    "catalogue is bound to workspace {existing}; refusing to open for workspace {want}"
                )));
            }
            None => {
                conn.execute(
                    "INSERT INTO catalogue_meta (key, value) VALUES ('workspace_id', ?1)",
                    [&want],
                )?;
            }
        }
        Ok(Self { conn, workspace_id })
    }

    /// Workspace this catalogue is bound to. Surfaced so the engine
    /// (and the CLI status handler) can assert configuration matches
    /// what the catalogue claims to know about.
    pub fn workspace_id(&self) -> Uuid {
        self.workspace_id
    }

    /// Insert or replace one record. Used for both "first time we
    /// see this file" and "we just finished uploading / downloading"
    /// transitions.
    pub fn upsert(&mut self, rec: &FileRecord) -> Result<()> {
        self.conn.execute(
            r#"INSERT INTO files (
                remote_file_id, remote_version_id, local_path,
                size_bytes, content_hash, status, pinned, updated_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)
            ON CONFLICT(remote_file_id) DO UPDATE SET
                remote_version_id = excluded.remote_version_id,
                local_path        = excluded.local_path,
                size_bytes        = excluded.size_bytes,
                content_hash      = excluded.content_hash,
                status            = excluded.status,
                pinned            = excluded.pinned,
                updated_at        = excluded.updated_at"#,
            params![
                rec.remote_file_id.to_string(),
                rec.remote_version_id.to_string(),
                rec.local_path.to_string_lossy().to_string(),
                rec.size_bytes as i64,
                rec.content_hash.to_vec(),
                rec.status.as_str(),
                rec.pinned as i32,
                rec.updated_at.to_rfc3339(),
            ],
        )?;
        Ok(())
    }

    /// Repoint a record's `local_path`. Used by the engine to move a
    /// row out of the way when a rename arrives at a path that is
    /// already tracked by a different file -- without this the
    /// downstream upsert would violate the schema's UNIQUE(local_path)
    /// constraint.
    pub fn set_local_path(&mut self, remote_file_id: Uuid, new_path: &Path) -> Result<()> {
        let now = chrono::Utc::now().to_rfc3339();
        self.conn.execute(
            "UPDATE files SET local_path = ?1, updated_at = ?2 WHERE remote_file_id = ?3",
            params![
                new_path.to_string_lossy().to_string(),
                now,
                remote_file_id.to_string()
            ],
        )?;
        Ok(())
    }

    /// Mark a record's [`SyncStatus`]. Cheap path used by the engine
    /// loop when transitioning a file in-flight / back to up_to_date.
    pub fn set_status(&mut self, remote_file_id: Uuid, status: SyncStatus) -> Result<()> {
        let now = chrono::Utc::now().to_rfc3339();
        self.conn.execute(
            "UPDATE files SET status = ?1, updated_at = ?2 WHERE remote_file_id = ?3",
            params![status.as_str(), now, remote_file_id.to_string()],
        )?;
        Ok(())
    }

    /// Update `status`, `content_hash`, and `size_bytes` atomically.
    /// Used by the engine when a local file change is detected: the
    /// state machine needs to flip status (e.g. `UpToDate -> LocalDirty`)
    /// *and* refresh the catalogue's view of the on-disk bytes so the
    /// dedup-against-stale-hash short-circuit in `handle_local`
    /// continues to work for follow-up events. Without this, an
    /// A -> B -> A revert would be silently missed because the
    /// catalogue would still hold hash A from before the first edit.
    pub fn set_local_state(
        &mut self,
        remote_file_id: Uuid,
        status: SyncStatus,
        content_hash: [u8; 32],
        size_bytes: u64,
    ) -> Result<()> {
        let now = chrono::Utc::now().to_rfc3339();
        self.conn.execute(
            r#"UPDATE files
                  SET status = ?1,
                      content_hash = ?2,
                      size_bytes = ?3,
                      updated_at = ?4
                WHERE remote_file_id = ?5"#,
            params![
                status.as_str(),
                content_hash.as_slice(),
                size_bytes as i64,
                now,
                remote_file_id.to_string()
            ],
        )?;
        Ok(())
    }

    /// Look up by remote file id.
    pub fn get(&self, remote_file_id: Uuid) -> Result<Option<FileRecord>> {
        let mut stmt = self.conn.prepare(
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at
             FROM files WHERE remote_file_id = ?1",
        )?;
        let row = stmt
            .query_row(params![remote_file_id.to_string()], row_to_record)
            .optional()?;
        Ok(row)
    }

    /// Look up by local path.
    pub fn by_local_path(&self, path: &Path) -> Result<Option<FileRecord>> {
        let mut stmt = self.conn.prepare(
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at
             FROM files WHERE local_path = ?1",
        )?;
        let row = stmt
            .query_row(params![path.to_string_lossy().to_string()], row_to_record)
            .optional()?;
        Ok(row)
    }

    /// Read the last-applied changefeed cursor for `workspace_id`.
    /// `0` is returned when no cursor has been persisted yet, which
    /// is also the wire value sync clients pass on first connect.
    pub fn get_cursor(&self, workspace_id: Uuid) -> Result<i64> {
        let mut stmt = self
            .conn
            .prepare("SELECT cursor FROM workspace_cursors WHERE workspace_id = ?1")?;
        Ok(stmt
            .query_row(params![workspace_id.to_string()], |r| r.get::<_, i64>(0))
            .optional()?
            .unwrap_or(0))
    }

    /// Persist the last-applied changefeed cursor for `workspace_id`.
    pub fn set_cursor(&mut self, workspace_id: Uuid, cursor: i64) -> Result<()> {
        self.conn.execute(
            r#"INSERT INTO workspace_cursors (workspace_id, cursor)
               VALUES (?1, ?2)
               ON CONFLICT(workspace_id) DO UPDATE SET cursor = excluded.cursor"#,
            params![workspace_id.to_string(), cursor],
        )?;
        Ok(())
    }

    /// Iterate every record in the catalogue. Used by the LRU eviction
    /// path in the offline-cache crate (and by integration tests).
    pub fn list_all(&self) -> Result<Vec<FileRecord>> {
        let mut stmt = self.conn.prepare(
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at
             FROM files ORDER BY updated_at ASC",
        )?;
        let rows = stmt.query_map([], row_to_record)?;
        let mut out = Vec::new();
        for r in rows {
            out.push(r?);
        }
        Ok(out)
    }
}

fn row_to_record(row: &rusqlite::Row<'_>) -> rusqlite::Result<FileRecord> {
    let remote_file_id_s: String = row.get(0)?;
    let remote_version_id_s: String = row.get(1)?;
    let local_path_s: String = row.get(2)?;
    let size: i64 = row.get(3)?;
    let hash_bytes: Vec<u8> = row.get(4)?;
    let status_s: String = row.get(5)?;
    let pinned: i32 = row.get(6)?;
    let updated_s: String = row.get(7)?;

    // The schema guarantees `content_hash` is a BLOB sized exactly
    // 32 bytes. Any other size means the row was written by a corrupt
    // or out-of-date SDK; surface that loudly so the operator can
    // re-sync, but degrade to the all-zero sentinel so the catalogue
    // remains openable. An all-zero hash is the same value used for
    // "not yet downloaded" placeholders, so the engine treats this row
    // as needing a fresh fetch from the server -- a safe fallback.
    let mut hash = [0u8; 32];
    if hash_bytes.len() == 32 {
        hash.copy_from_slice(&hash_bytes);
    } else {
        warn!(
            remote_file_id = %remote_file_id_s,
            actual_len = hash_bytes.len(),
            "catalogue content_hash is not 32 bytes; treating row as not-downloaded"
        );
    }
    let parse_uuid = |s: &str| {
        Uuid::parse_str(s).map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))
    };
    let parse_dt = |s: &str| -> rusqlite::Result<chrono::DateTime<chrono::Utc>> {
        chrono::DateTime::parse_from_rfc3339(s)
            .map(|dt| dt.with_timezone(&chrono::Utc))
            .map_err(|e| rusqlite::Error::ToSqlConversionFailure(Box::new(e)))
    };
    Ok(FileRecord {
        remote_file_id: parse_uuid(&remote_file_id_s)?,
        remote_version_id: parse_uuid(&remote_version_id_s)?,
        local_path: PathBuf::from(local_path_s),
        size_bytes: size as u64,
        content_hash: hash,
        status: SyncStatus::parse(&status_s),
        pinned: pinned != 0,
        updated_at: parse_dt(&updated_s)?,
    })
}

const SCHEMA: &str = r#"
CREATE TABLE IF NOT EXISTS files (
    remote_file_id     TEXT PRIMARY KEY,
    remote_version_id  TEXT NOT NULL,
    local_path         TEXT NOT NULL UNIQUE,
    size_bytes         INTEGER NOT NULL,
    content_hash       BLOB NOT NULL CHECK(length(content_hash) = 32),
    status             TEXT NOT NULL,
    pinned             INTEGER NOT NULL DEFAULT 0,
    updated_at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_files_status ON files(status);
CREATE INDEX IF NOT EXISTS idx_files_updated_at ON files(updated_at);

CREATE TABLE IF NOT EXISTS workspace_cursors (
    workspace_id TEXT PRIMARY KEY,
    cursor       INTEGER NOT NULL
);

-- Stores the workspace this catalogue is exclusively bound to. The
-- catalogue rejects opens for any other workspace_id; this is the
-- enforcement point for the "one catalogue file per workspace"
-- invariant relied on by every files-table query (which is keyed by
-- remote_file_id / local_path, NOT by workspace).
CREATE TABLE IF NOT EXISTS catalogue_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
"#;

#[cfg(test)]
mod tests {
    use super::*;

    fn rec(name: &str) -> FileRecord {
        FileRecord {
            remote_file_id: Uuid::new_v4(),
            remote_version_id: Uuid::new_v4(),
            local_path: PathBuf::from(format!("/tmp/{name}")),
            size_bytes: 42,
            content_hash: [1u8; 32],
            status: SyncStatus::UpToDate,
            pinned: false,
            updated_at: chrono::Utc::now(),
        }
    }

    #[test]
    fn open_creates_schema_idempotently() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("catalogue.db");
        let ws = Uuid::new_v4();
        let _ = Catalogue::open(&p, ws).unwrap();
        let _ = Catalogue::open(&p, ws).unwrap(); // Re-open same workspace must succeed.
    }

    #[test]
    fn open_rejects_mismatched_workspace_binding() {
        let tmp = tempfile::tempdir().unwrap();
        let p = tmp.path().join("catalogue.db");
        let ws_a = Uuid::new_v4();
        let ws_b = Uuid::new_v4();
        let _ = Catalogue::open(&p, ws_a).unwrap();
        // Use a hand-rolled match so we don't have to require `Debug`
        // on the catalogue handle just to satisfy `expect_err`.
        let err = match Catalogue::open(&p, ws_b) {
            Err(e) => e,
            Ok(_) => panic!("opening the same catalogue path for a different workspace must fail"),
        };
        let msg = format!("{err}");
        assert!(
            msg.contains(&ws_a.to_string()) && msg.contains(&ws_b.to_string()),
            "error message must surface both workspace ids: {msg}"
        );
    }

    #[test]
    fn upsert_and_get_round_trip() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let mut r = rec("a.txt");
        cat.upsert(&r).unwrap();
        let got = cat.get(r.remote_file_id).unwrap().unwrap();
        assert_eq!(got.size_bytes, r.size_bytes);
        assert_eq!(got.local_path, r.local_path);
        assert_eq!(got.content_hash, r.content_hash);
        assert_eq!(got.status, SyncStatus::UpToDate);
        // Update path: change status, re-upsert, re-read.
        r.status = SyncStatus::LocalDirty;
        cat.upsert(&r).unwrap();
        let got = cat.get(r.remote_file_id).unwrap().unwrap();
        assert_eq!(got.status, SyncStatus::LocalDirty);
    }

    #[test]
    fn by_local_path_matches_upsert() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let r = rec("b.bin");
        cat.upsert(&r).unwrap();
        let got = cat.by_local_path(&r.local_path).unwrap().unwrap();
        assert_eq!(got.remote_file_id, r.remote_file_id);
    }

    #[test]
    fn cursor_round_trip() {
        let tmp = tempfile::tempdir().unwrap();
        let ws = Uuid::new_v4();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), ws).unwrap();
        assert_eq!(cat.get_cursor(ws).unwrap(), 0);
        cat.set_cursor(ws, 42).unwrap();
        assert_eq!(cat.get_cursor(ws).unwrap(), 42);
        cat.set_cursor(ws, 100).unwrap();
        assert_eq!(cat.get_cursor(ws).unwrap(), 100);
    }

    #[test]
    fn list_all_orders_by_updated_at() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let mut r1 = rec("a");
        r1.updated_at = chrono::Utc::now() - chrono::Duration::seconds(10);
        let r2 = rec("b");
        cat.upsert(&r1).unwrap();
        cat.upsert(&r2).unwrap();
        let all = cat.list_all().unwrap();
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].remote_file_id, r1.remote_file_id);
    }
}
