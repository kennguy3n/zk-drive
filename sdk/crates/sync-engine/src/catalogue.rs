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
    /// Remote change waiting to be downloaded (content update,
    /// rename, or move). Kept distinct from [`SyncStatus::RemoteDeleted`]
    /// so the downloader (PR5) does not have to re-fetch the file's
    /// metadata to discover whether the remote-side change was an
    /// update or a tombstone -- a critical disambiguation, because
    /// for a deletion it must unlink the local file rather than
    /// download new bytes.
    RemoteDirty,
    /// Remote object was deleted; the downloader should unlink the
    /// local copy (subject to the conflict policy if local has
    /// pending changes). Distinct from [`SyncStatus::RemoteDirty`]
    /// for the same reason [`SyncStatus::LocalDeleted`] is distinct
    /// from [`SyncStatus::LocalDirty`]: the action diverges from
    /// 'transfer content' to 'remove the entry'.
    RemoteDeleted,
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
            SyncStatus::RemoteDeleted => "remote_deleted",
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
            // Server already executed a tombstone; recreating
            // locally is a fresh upload, not a revival of the
            // pre-delete row. Mark LocalDirty so the upload flow
            // pushes the new bytes (under whatever new resource id
            // the server allocates).
            SyncStatus::RemoteDeleted => SyncStatus::LocalDirty,
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
            // Both sides agree on a delete. Pick RemoteDeleted as
            // the convergence point for parity with
            // `next_on_remote_delete(LocalDeleted)`; the upload flow
            // skips pushing a tombstone that the server has already
            // executed.
            SyncStatus::RemoteDeleted => SyncStatus::RemoteDeleted,
            SyncStatus::RemoteDirty | SyncStatus::Conflict | SyncStatus::InFlight => {
                SyncStatus::Conflict
            }
        }
    }

    /// Mirror of [`Self::next_on_local_upsert`] / [`Self::next_on_local_delete`]
    /// for remote-side *content* changes (catch-up page or live
    /// WebSocket frame announcing a create, update, rename, or
    /// move). Use [`Self::next_on_remote_delete`] for tombstones.
    ///
    /// Truth table (current -> next):
    ///
    /// | current       | next                                           |
    /// |---------------|------------------------------------------------|
    /// | UpToDate      | RemoteDirty                                    |
    /// | RemoteDirty   | RemoteDirty                                    |
    /// | RemoteDeleted | RemoteDirty (remote resurrected what it killed)|
    /// | Evicted       | RemoteDirty                                    |
    /// | LocalDirty    | Conflict (local pending + remote change)       |
    /// | LocalDeleted  | Conflict (local tombstone + remote change)     |
    /// | Conflict      | Conflict                                       |
    /// | InFlight      | Conflict (transfer in progress + remote chg)   |
    pub fn next_on_remote_change(self) -> Self {
        match self {
            SyncStatus::UpToDate | SyncStatus::Evicted => SyncStatus::RemoteDirty,
            SyncStatus::RemoteDirty => SyncStatus::RemoteDirty,
            // A remote create after a remote delete (e.g. user
            // restored the file on another device) needs to escape
            // the RemoteDeleted state -- otherwise the downloader
            // would still treat the row as a tombstone and unlink
            // the local copy when it finally syncs.
            SyncStatus::RemoteDeleted => SyncStatus::RemoteDirty,
            SyncStatus::LocalDirty
            | SyncStatus::LocalDeleted
            | SyncStatus::Conflict
            | SyncStatus::InFlight => SyncStatus::Conflict,
        }
    }

    /// Catalogue status to persist when the change feed announces a
    /// remote *tombstone* (file/folder deleted on the server). The
    /// downloader (PR5) must consume this distinctly from
    /// [`SyncStatus::RemoteDirty`] because the action is to unlink
    /// the local file, not to fetch new content -- without this
    /// split, [`RemoteEvent::FileDeleted`] and [`RemoteEvent::FileUpdated`]
    /// would land on indistinguishable catalogue rows and the
    /// downloader would have to re-stat the server to recover the
    /// op, defeating the point of the change feed.
    ///
    /// Truth table (current -> next):
    ///
    /// | current       | next                                           |
    /// |---------------|------------------------------------------------|
    /// | UpToDate      | RemoteDeleted                                  |
    /// | RemoteDirty   | RemoteDeleted (remote deletion supersedes update) |
    /// | RemoteDeleted | RemoteDeleted                                  |
    /// | Evicted       | RemoteDeleted                                  |
    /// | LocalDirty    | Conflict (local pending + remote delete)       |
    /// | LocalDeleted  | RemoteDeleted (both sides agree; tombstone wins) |
    /// | Conflict      | Conflict                                       |
    /// | InFlight      | Conflict (transfer in progress + remote delete) |
    pub fn next_on_remote_delete(self) -> Self {
        match self {
            SyncStatus::UpToDate
            | SyncStatus::RemoteDirty
            | SyncStatus::RemoteDeleted
            | SyncStatus::Evicted => SyncStatus::RemoteDeleted,
            // Both sides converged on a delete -- no conflict, the
            // row is just a tombstone now. Picking RemoteDeleted
            // (rather than LocalDeleted) means the downloader owns
            // the cleanup, which is the right side because the
            // server has already executed its delete; the upload
            // flow would only push a redundant tombstone.
            SyncStatus::LocalDeleted => SyncStatus::RemoteDeleted,
            SyncStatus::LocalDirty | SyncStatus::Conflict | SyncStatus::InFlight => {
                SyncStatus::Conflict
            }
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
            "remote_deleted" => SyncStatus::RemoteDeleted,
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
    /// Wall-clock time the file was last opened / consumed by the
    /// user. Distinct from [`Self::updated_at`] because reads do not
    /// (and must not) mutate the sync-state machine -- a read should
    /// not, say, surface a row as "recently changed" to the change
    /// feed -- but the LRU eviction policy needs to distinguish
    /// "file the user touches every day" from "file the user
    /// downloaded once and forgot about". Defaults to `updated_at`
    /// on the first migration for catalogues that predate this
    /// column.
    pub last_accessed_at: chrono::DateTime<chrono::Utc>,
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
        let mut conn = rusqlite::Connection::open(path)?;
        conn.pragma_update(None, "journal_mode", "WAL")?;
        conn.pragma_update(None, "synchronous", "NORMAL")?;
        conn.pragma_update(None, "foreign_keys", "ON")?;
        conn.execute_batch(SCHEMA)?;
        // Idempotent additive migrations. SQLite has no "ADD COLUMN IF
        // NOT EXISTS" so we probe each column with PRAGMA and only
        // ALTER on miss. New columns must declare DEFAULT values so
        // existing rows are reachable through the new query path on
        // first open after an SDK upgrade.
        //
        // The ALTER + the backfill UPDATE run inside one transaction
        // so a crash between the structural change and the backfill
        // can never leave the catalogue with a column full of empty
        // strings that `parse_dt("")` would later reject. The
        // backfill is ALSO run unconditionally outside the
        // `has_column` guard: if the column already exists from an
        // earlier open of the same SDK version we still want to
        // sweep up any `''` rows left behind by a pre-transaction
        // build of this code. That second sweep is a one-row UPDATE
        // hitting zero rows in the happy case (every row has a
        // valid RFC3339 timestamp), so the cost is a single index
        // probe per open.
        if !has_column(&conn, "files", "last_accessed_at")? {
            let tx = conn.transaction()?;
            // ALTER TABLE ... ADD COLUMN can't use non-constant
            // DEFAULTs (e.g. CURRENT_TIMESTAMP) in older SQLite
            // versions and the literal default makes the column
            // useless for LRU on existing rows. Backfill from
            // updated_at after the structural add so every existing
            // row starts with a non-degenerate LRU position.
            tx.execute_batch(
                "ALTER TABLE files ADD COLUMN last_accessed_at TEXT NOT NULL DEFAULT '';",
            )?;
            tx.execute(
                "UPDATE files SET last_accessed_at = updated_at WHERE last_accessed_at = ''",
                [],
            )?;
            tx.execute_batch(
                "CREATE INDEX IF NOT EXISTS idx_files_last_accessed_at ON files(last_accessed_at);",
            )?;
            tx.commit()?;
        } else {
            // Crash-recovery sweep: if a prior open ran the
            // pre-transactional migration code and was killed
            // between the ALTER and the UPDATE, every row touched
            // since carries `last_accessed_at = ''`. Empty strings
            // would later fail `parse_dt` and break every catalogue
            // query. This UPDATE is a no-op when the column is
            // populated and self-heals when it isn't.
            conn.execute(
                "UPDATE files SET last_accessed_at = updated_at WHERE last_accessed_at = ''",
                [],
            )?;
            // The index may also be missing if a *very* old build
            // shipped without it. CREATE INDEX IF NOT EXISTS is
            // idempotent so calling it unconditionally is free.
            conn.execute_batch(
                "CREATE INDEX IF NOT EXISTS idx_files_last_accessed_at ON files(last_accessed_at);",
            )?;
        }
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
                size_bytes, content_hash, status, pinned, updated_at,
                last_accessed_at
            ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9)
            ON CONFLICT(remote_file_id) DO UPDATE SET
                remote_version_id = excluded.remote_version_id,
                local_path        = excluded.local_path,
                size_bytes        = excluded.size_bytes,
                content_hash      = excluded.content_hash,
                status            = excluded.status,
                pinned            = excluded.pinned,
                updated_at        = excluded.updated_at,
                last_accessed_at  = excluded.last_accessed_at"#,
            params![
                rec.remote_file_id.to_string(),
                rec.remote_version_id.to_string(),
                rec.local_path.to_string_lossy().to_string(),
                rec.size_bytes as i64,
                rec.content_hash.to_vec(),
                rec.status.as_str(),
                rec.pinned as i32,
                rec.updated_at.to_rfc3339(),
                rec.last_accessed_at.to_rfc3339(),
            ],
        )?;
        Ok(())
    }

    /// Toggle the offline-pin bit on a single row. Pinned files are
    /// excluded from LRU eviction; the upload / download flows are
    /// otherwise unaffected (pinning is a cache-residency hint, not
    /// a sync-state mutation). Returns the prior pinned value so the
    /// caller can distinguish a real change from a no-op.
    pub fn set_pinned(&mut self, remote_file_id: Uuid, pinned: bool) -> Result<Option<bool>> {
        let prior: Option<i32> = self
            .conn
            .query_row(
                "SELECT pinned FROM files WHERE remote_file_id = ?1",
                params![remote_file_id.to_string()],
                |r| r.get(0),
            )
            .optional()?;
        let Some(prior) = prior else { return Ok(None) };
        // Pinning does NOT bump updated_at: the change-feed is for
        // remote-visible state and pinning is purely a local cache
        // hint. Bumping updated_at would falsely advance the upload
        // queue's LocalDirty-ordered scan.
        self.conn.execute(
            "UPDATE files SET pinned = ?1 WHERE remote_file_id = ?2",
            params![pinned as i32, remote_file_id.to_string()],
        )?;
        Ok(Some(prior != 0))
    }

    /// Bump `last_accessed_at` to the current wall clock for a single
    /// row. Called by the offline read path on every download / open
    /// so the LRU evictor can distinguish hot from cold rows. A
    /// missing row is a no-op (returns false) -- the caller is the
    /// downloader, and a download for a non-catalogued file means
    /// either a stale request or a row that was just evicted; either
    /// way the right behaviour is to skip silently rather than
    /// resurrecting a tombstoned row by side effect.
    pub fn touch_access(&mut self, remote_file_id: Uuid) -> Result<bool> {
        let now = rfc3339_now();
        let n = self.conn.execute(
            "UPDATE files SET last_accessed_at = ?1 WHERE remote_file_id = ?2",
            params![now, remote_file_id.to_string()],
        )?;
        Ok(n > 0)
    }

    /// Total on-disk byte footprint of every row whose content is
    /// currently materialised locally. The sum excludes rows in
    /// transient or tombstone states (`LocalDeleted`, `RemoteDeleted`,
    /// `Evicted`, `InFlight`) because those rows either have no
    /// content on disk or are mid-transfer and may already have a
    /// reserved-but-not-yet-occupied size. Used by the LRU evictor
    /// to decide whether the catalogue is over quota.
    pub fn total_cached_bytes(&self) -> Result<u64> {
        let mut stmt = self.conn.prepare(
            "SELECT COALESCE(SUM(size_bytes), 0) FROM files
             WHERE status IN ('up_to_date', 'local_dirty', 'remote_dirty', 'conflict')",
        )?;
        let total: i64 = stmt.query_row([], |r| r.get(0))?;
        Ok(total.max(0) as u64)
    }

    /// Returns rows eligible for LRU eviction, oldest-access first.
    /// Eligibility rules (the engine's LRU evictor relies on every
    /// one of these; changing the SQL here without updating
    /// [`crate::eviction`] is a bug):
    ///
    ///   * `pinned = 0` -- explicitly-pinned rows are kept.
    ///   * `status = up_to_date` ONLY. Evicting any other status
    ///     would lose data: `LocalDirty` has bytes the server
    ///     hasn't seen, `RemoteDirty` has bytes we haven't fetched
    ///     (evicting clears the file we were about to overwrite
    ///     anyway, but it also drops the metadata pointer to the
    ///     server's new version), `Conflict` needs human
    ///     resolution, `InFlight` is racing against the transfer
    ///     loop, and `Evicted` is already a tombstone.
    ///   * `size_bytes > 0` -- a zero-byte file is meaningless to
    ///     evict (no disk to reclaim) and the dedup placeholder
    ///     paths from the catch-up flow are typically zero-byte;
    ///     skipping them avoids work that produces no benefit.
    ///
    /// `limit` caps the page so the evictor doesn't load the whole
    /// catalogue into memory on a single pass; callers iterate
    /// pages until quota is met.
    pub fn eviction_candidates(&self, limit: usize) -> Result<Vec<FileRecord>> {
        self.eviction_candidates_after(None, limit)
    }

    /// Variant of [`Self::eviction_candidates`] that paginates by
    /// `last_accessed_at` cursor. Returns rows whose
    /// `last_accessed_at` is **strictly greater than** `cursor`, in
    /// the same order. The cursor is the RFC3339 string of the last
    /// row returned by the previous page; pass `None` for the first
    /// page.
    ///
    /// Why a separate method: with plain `LIMIT N`, if every row in
    /// a page is stuck (unlink failed -- e.g. EBUSY on an mmap'd
    /// file, EPERM on a read-only fs) the next iteration of the
    /// outer loop re-queries the SAME stuck rows because they still
    /// have the oldest `last_accessed_at` (no rows were transitioned
    /// to Evicted). Without the cursor the evictor stalls even when
    /// the workspace has plenty of fresher rows we *could* evict.
    /// With keyset pagination on `last_accessed_at`, every iteration
    /// moves strictly forward in time, so stuck rows can't block
    /// progress.
    ///
    /// Strict-greater-than (`>`) is intentional: ties on
    /// `last_accessed_at` are possible (the test fixture's bulk
    /// loader writes microseconds-apart, and `chrono` rounds to
    /// nanoseconds via RFC3339) but are not load-bearing for LRU
    /// correctness. Skipping ties costs at most a handful of rows
    /// per pass and prevents an infinite loop where the cursor
    /// never advances.
    pub fn eviction_candidates_after(
        &self,
        cursor: Option<&str>,
        limit: usize,
    ) -> Result<Vec<FileRecord>> {
        let mut stmt;
        let rows = if let Some(c) = cursor {
            stmt = self.conn.prepare(
                "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
                 FROM files
                 WHERE pinned = 0
                   AND status = 'up_to_date'
                   AND size_bytes > 0
                   AND last_accessed_at > ?1
                 ORDER BY last_accessed_at ASC
                 LIMIT ?2",
            )?;
            stmt.query_map(params![c, limit as i64], row_to_record)?
        } else {
            stmt = self.conn.prepare(
                "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
                 FROM files
                 WHERE pinned = 0
                   AND status = 'up_to_date'
                   AND size_bytes > 0
                 ORDER BY last_accessed_at ASC
                 LIMIT ?1",
            )?;
            stmt.query_map(params![limit as i64], row_to_record)?
        };
        let mut out = Vec::new();
        for r in rows {
            out.push(r?);
        }
        Ok(out)
    }

    /// Count rows in a given status. Used by the CLI status output
    /// ("3 pending uploads, 1 conflict") so the operator can see at
    /// a glance whether the sync is keeping up. A SELECT COUNT(*) is
    /// cheap on the `idx_files_status` index.
    pub fn count_by_status(&self, status: SyncStatus) -> Result<u64> {
        let n: i64 = self.conn.query_row(
            "SELECT COUNT(*) FROM files WHERE status = ?1",
            params![status.as_str()],
            |r| r.get(0),
        )?;
        Ok(n.max(0) as u64)
    }

    /// Count pinned rows. Surface for the CLI status output; cheap
    /// because the `pinned` column has only two distinct values
    /// (and SQLite can scan via the WHERE clause).
    pub fn count_pinned(&self) -> Result<u64> {
        let n: i64 =
            self.conn
                .query_row("SELECT COUNT(*) FROM files WHERE pinned = 1", [], |r| {
                    r.get(0)
                })?;
        Ok(n.max(0) as u64)
    }

    /// Single-query aggregate used by the status snapshot. Returns
    /// `(total_cached_bytes, pinned_count, per_status_counts)`.
    ///
    /// Why a dedicated method rather than calling
    /// [`Self::total_cached_bytes`] + [`Self::count_pinned`] +
    /// `count_by_status` * 8: that previous shape issued 10
    /// independent `SELECT` statements with 10 separate index probes
    /// and 10 round-trips through rusqlite's prepared-statement
    /// cache. With this aggregate, SQLite scans the table (or the
    /// `idx_files_status` index) exactly once and returns one row
    /// per distinct status. For a catalogue with ~10k files this
    /// drops the snapshot path from milliseconds to tens of
    /// microseconds; for the Tauri tray that polls this every few
    /// seconds the savings compound.
    ///
    /// Atomicity: a single SQLite statement runs inside a transient
    /// read transaction, so the returned counts are mutually
    /// consistent. The 10-statement implementation could in
    /// principle observe a row moving between statuses between
    /// queries (the catalogue mutex prevents writes from another
    /// thread of the same SDK process, but a separate process
    /// holding its own connection -- like the CLI status command
    /// running alongside a live agent -- could mutate between
    /// queries). This aggregate eliminates that window entirely.
    ///
    /// The `CASE WHEN status IN (...)` filter for `cached_bytes`
    /// must stay in sync with the equivalent filter in
    /// [`Self::total_cached_bytes`]; both definitions exclude
    /// `LocalDeleted`, `RemoteDeleted`, `Evicted`, and `InFlight`
    /// because those rows either have no content on disk or are
    /// mid-transfer with a reserved-but-not-yet-occupied size.
    pub fn status_aggregate(&self) -> Result<(u64, u64, crate::status::SyncStatusCounts)> {
        use crate::status::SyncStatusCounts;
        let mut stmt = self.conn.prepare(
            "SELECT
                status,
                COUNT(*) AS cnt,
                COALESCE(SUM(CASE WHEN pinned = 1 THEN 1 ELSE 0 END), 0) AS pinned_cnt,
                COALESCE(SUM(
                    CASE WHEN status IN ('up_to_date', 'local_dirty', 'remote_dirty', 'conflict')
                         THEN size_bytes ELSE 0 END
                ), 0) AS sum_size
             FROM files
             GROUP BY status",
        )?;
        let rows = stmt.query_map([], |r| {
            let status: String = r.get(0)?;
            let cnt: i64 = r.get(1)?;
            let pinned_cnt: i64 = r.get(2)?;
            let sum_size: i64 = r.get(3)?;
            Ok((status, cnt, pinned_cnt, sum_size))
        })?;
        let mut cached: u64 = 0;
        let mut pinned: u64 = 0;
        let mut counts = SyncStatusCounts::default();
        for row in rows {
            let (status_s, cnt, pinned_cnt, sum_size) = row?;
            let cnt = cnt.max(0) as u64;
            pinned = pinned.saturating_add(pinned_cnt.max(0) as u64);
            cached = cached.saturating_add(sum_size.max(0) as u64);
            match status_s.as_str() {
                "up_to_date" => counts.up_to_date = cnt,
                "local_dirty" => counts.local_dirty = cnt,
                "local_deleted" => counts.local_deleted = cnt,
                "remote_dirty" => counts.remote_dirty = cnt,
                "remote_deleted" => counts.remote_deleted = cnt,
                "conflict" => counts.conflict = cnt,
                "in_flight" => counts.in_flight = cnt,
                "evicted" => counts.evicted = cnt,
                // An unknown status string means the catalogue was
                // written by a newer SDK version that knows a status
                // this build doesn't. The aggregate quietly drops it
                // from the per-variant counters but still includes
                // its rows in `cached` / `pinned` because the SQL
                // already classified those at the row level. Logging
                // here would be too noisy for what is fundamentally
                // a forward-compat scenario.
                _ => {}
            }
        }
        Ok((cached, pinned, counts))
    }

    /// Repoint a record's `local_path`. Used by the engine to move a
    /// row out of the way when a rename arrives at a path that is
    /// already tracked by a different file -- without this the
    /// downstream upsert would violate the schema's UNIQUE(local_path)
    /// constraint.
    pub fn set_local_path(&mut self, remote_file_id: Uuid, new_path: &Path) -> Result<()> {
        let now = rfc3339_now();
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
        let now = rfc3339_now();
        self.conn.execute(
            "UPDATE files SET status = ?1, updated_at = ?2 WHERE remote_file_id = ?3",
            params![status.as_str(), now, remote_file_id.to_string()],
        )?;
        Ok(())
    }

    /// Run `f` inside an explicit `BEGIN IMMEDIATE` ... `COMMIT`
    /// transaction. Multi-step engine operations (e.g. the Rename
    /// handler which performs `set_local_path` + `set_status` +
    /// `upsert` in sequence) call into this so a failure halfway
    /// through rolls every mutation back as a unit. Without it the
    /// individual `Catalogue::*` methods only have implicit
    /// auto-commit semantics, which would leave the catalogue
    /// half-displaced if (say) step 2 hits ENOSPC.
    ///
    /// `f` operates against the same `&mut Catalogue` -- the
    /// underlying connection joins the open transaction, so no
    /// special routing through a `Transaction` handle is needed.
    /// Nested `with_txn` calls would error: SQLite doesn't permit
    /// nested transactions without SAVEPOINTs, and the catalogue's
    /// engine call sites never nest.
    pub fn with_txn<F, T>(&mut self, f: F) -> Result<T>
    where
        F: FnOnce(&mut Self) -> Result<T>,
    {
        self.conn.execute_batch("BEGIN IMMEDIATE")?;
        match f(self) {
            Ok(v) => {
                self.conn.execute_batch("COMMIT")?;
                Ok(v)
            }
            Err(e) => {
                // ROLLBACK best-effort: if even the rollback itself
                // fails the connection is in an indeterminate state
                // and the caller will surface the original error
                // anyway; SQLite WAL recovery handles the partial
                // transaction on the next open.
                let _ = self.conn.execute_batch("ROLLBACK");
                Err(e)
            }
        }
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
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
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
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
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
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
             FROM files ORDER BY updated_at ASC",
        )?;
        let rows = stmt.query_map([], row_to_record)?;
        let mut out = Vec::new();
        for r in rows {
            out.push(r?);
        }
        Ok(out)
    }

    /// Iterate every row whose `status` matches one of the given
    /// values. Used by the engine's upload loop to walk the
    /// "pending uploads queue" (rows in `LocalDirty` / `LocalDeleted`)
    /// in oldest-first order so a slow uploader doesn't starve old
    /// edits behind newer ones. Returns rows in `updated_at` ASC.
    pub fn list_by_status(&self, statuses: &[SyncStatus]) -> Result<Vec<FileRecord>> {
        if statuses.is_empty() {
            return Ok(Vec::new());
        }
        // SQLite has no array-binding for `WHERE status IN (?)`, so we
        // build a placeholder list of the right length and bind each
        // status string individually. Cap is the size of the
        // SyncStatus enum (8) so the dynamic SQL is bounded; no SQL
        // injection risk because we never substitute caller text.
        let placeholders = statuses.iter().map(|_| "?").collect::<Vec<_>>().join(",");
        let sql = format!(
            "SELECT remote_file_id, remote_version_id, local_path, size_bytes, content_hash, status, pinned, updated_at, last_accessed_at
             FROM files WHERE status IN ({placeholders}) ORDER BY updated_at ASC"
        );
        let mut stmt = self.conn.prepare(&sql)?;
        // `SyncStatus::as_str` returns `&'static str`. SQLite wants a
        // slice of `&dyn ToSql` and `str` itself isn't sized -- bind
        // through an intermediate `Vec<&'static str>` so each entry
        // is `&&'static str`, which IS sized and coerces to the
        // required trait object.
        let status_strs: Vec<&'static str> = statuses.iter().map(|s| s.as_str()).collect();
        let params_vec: Vec<&dyn rusqlite::ToSql> = status_strs
            .iter()
            .map(|s| s as &dyn rusqlite::ToSql)
            .collect();
        let rows = stmt.query_map(params_vec.as_slice(), row_to_record)?;
        let mut out = Vec::new();
        for r in rows {
            out.push(r?);
        }
        Ok(out)
    }
}

/// Returns true if `column` is present on `table` in the open
/// connection. Used by [`Catalogue::open`] for idempotent additive
/// migrations; SQLite has no `ADD COLUMN IF NOT EXISTS` so we probe
/// `PRAGMA table_info` instead. The query is intentionally lossy
/// (it ignores type / nullability / default) -- we only need to
/// know whether to issue the `ALTER TABLE`.
/// All timestamp columns in the catalogue are TEXT in RFC 3339
/// representation. SQLite compares TEXT lexicographically, which
/// produces correct chronological ordering ONLY if every stored
/// value uses the same timezone suffix format -- `chrono`'s
/// `to_rfc3339` formats with `+00:00` in UTC, whereas other tools
/// might write the same instant as `Z`. Because `Z` (0x5A) sorts
/// AFTER `+` (0x2B) in ASCII but BEFORE every digit, mixing the
/// two formats would silently corrupt the LRU ordering in
/// `eviction_candidates`. Every write path routes through this
/// helper so the invariant is enforced at the single point of
/// truth.
fn rfc3339_now() -> String {
    chrono::Utc::now().to_rfc3339()
}

fn has_column(conn: &rusqlite::Connection, table: &str, column: &str) -> Result<bool> {
    let sql = format!("PRAGMA table_info({table})");
    let mut stmt = conn.prepare(&sql)?;
    let mut rows = stmt.query([])?;
    while let Some(row) = rows.next()? {
        let name: String = row.get(1)?;
        if name == column {
            return Ok(true);
        }
    }
    Ok(false)
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
    // last_accessed_at is the 9th column (added in the PR5 offline
    // migration). Older callers that don't select it can still use
    // this function via SELECT *... but every call site in the
    // catalogue selects it explicitly above for clarity.
    let last_accessed_s: String = row.get(8)?;

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
        last_accessed_at: parse_dt(&last_accessed_s)?,
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
            last_accessed_at: chrono::Utc::now(),
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

    /// `with_txn` must commit on Ok and rollback on Err. The
    /// rollback path is the load-bearing property: the engine's
    /// Rename handler relies on it to undo `set_local_path` + an
    /// implicit row repoint as a unit when a downstream mutation
    /// fails.
    #[test]
    fn with_txn_commits_on_ok_and_rolls_back_on_err() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let r = rec("base.bin");
        cat.upsert(&r).unwrap();
        let id = r.remote_file_id;

        // Ok path: status flips and is visible after with_txn returns.
        cat.with_txn(|c| c.set_status(id, SyncStatus::LocalDirty))
            .unwrap();
        assert_eq!(
            cat.get(id).unwrap().unwrap().status,
            SyncStatus::LocalDirty,
            "with_txn must commit a successful mutation"
        );

        // Err path: status flip is rolled back when the closure returns Err.
        let err = cat
            .with_txn(|c| -> Result<()> {
                c.set_status(id, SyncStatus::Conflict)?;
                Err(crate::SyncError::Other("simulated mid-txn failure".into()))
            })
            .unwrap_err();
        assert!(format!("{err}").contains("simulated mid-txn failure"));
        assert_eq!(
            cat.get(id).unwrap().unwrap().status,
            SyncStatus::LocalDirty,
            "with_txn must roll back partial mutations on Err"
        );
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

    #[test]
    fn set_pinned_round_trip_and_idempotency() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let r = rec("p.bin");
        cat.upsert(&r).unwrap();
        // Initial: pinned=false. First set returns prior=false.
        assert_eq!(cat.set_pinned(r.remote_file_id, true).unwrap(), Some(false));
        assert!(cat.get(r.remote_file_id).unwrap().unwrap().pinned);
        // Idempotent re-pin returns prior=true (no change but report).
        assert_eq!(cat.set_pinned(r.remote_file_id, true).unwrap(), Some(true));
        // Unpin: prior=true.
        assert_eq!(cat.set_pinned(r.remote_file_id, false).unwrap(), Some(true));
        assert!(!cat.get(r.remote_file_id).unwrap().unwrap().pinned);
        // Missing row: None.
        let missing = Uuid::new_v4();
        assert_eq!(cat.set_pinned(missing, true).unwrap(), None);
    }

    #[test]
    fn touch_access_bumps_last_accessed_only() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let mut r = rec("t.bin");
        // Seed with an obviously-stale last_accessed_at.
        r.last_accessed_at = chrono::Utc::now() - chrono::Duration::hours(72);
        let original_updated = r.updated_at;
        cat.upsert(&r).unwrap();

        std::thread::sleep(std::time::Duration::from_millis(10));
        assert!(cat.touch_access(r.remote_file_id).unwrap());

        let after = cat.get(r.remote_file_id).unwrap().unwrap();
        assert!(
            after.last_accessed_at > r.last_accessed_at,
            "last_accessed_at must move forward"
        );
        // CRITICAL: updated_at must NOT move; the change feed
        // would otherwise treat a read as a write.
        assert_eq!(after.updated_at, original_updated);

        // Missing row: false, no panic.
        assert!(!cat.touch_access(Uuid::new_v4()).unwrap());
    }

    #[test]
    fn total_cached_bytes_excludes_terminal_statuses() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let mut up = rec("a");
        up.size_bytes = 100;
        up.status = SyncStatus::UpToDate;
        let mut dirty = rec("b");
        dirty.size_bytes = 50;
        dirty.status = SyncStatus::LocalDirty;
        let mut evict = rec("c");
        evict.size_bytes = 999;
        evict.status = SyncStatus::Evicted;
        let mut deleted = rec("d");
        deleted.size_bytes = 999;
        deleted.status = SyncStatus::RemoteDeleted;
        cat.upsert(&up).unwrap();
        cat.upsert(&dirty).unwrap();
        cat.upsert(&evict).unwrap();
        cat.upsert(&deleted).unwrap();
        // 100 (UpToDate) + 50 (LocalDirty) -- the other two excluded.
        assert_eq!(cat.total_cached_bytes().unwrap(), 150);
    }

    #[test]
    fn eviction_candidates_respect_pinning_status_and_size() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let now = chrono::Utc::now();
        let mk = |name: &str, pinned, status, size, age_s: i64| {
            let mut r = rec(name);
            r.pinned = pinned;
            r.status = status;
            r.size_bytes = size;
            r.last_accessed_at = now - chrono::Duration::seconds(age_s);
            r
        };
        let pinned = mk("p", true, SyncStatus::UpToDate, 10, 1000);
        let dirty = mk("d", false, SyncStatus::LocalDirty, 10, 999);
        let zero = mk("z", false, SyncStatus::UpToDate, 0, 998);
        let oldest = mk("o", false, SyncStatus::UpToDate, 10, 500);
        let newest = mk("n", false, SyncStatus::UpToDate, 10, 1);
        cat.upsert(&pinned).unwrap();
        cat.upsert(&dirty).unwrap();
        cat.upsert(&zero).unwrap();
        cat.upsert(&oldest).unwrap();
        cat.upsert(&newest).unwrap();

        let cands = cat.eviction_candidates(10).unwrap();
        let ids: Vec<_> = cands.iter().map(|r| r.remote_file_id).collect();
        // Only `oldest` and `newest` qualify -- pinned/dirty/zero
        // are filtered out at the SQL layer.
        assert_eq!(ids, vec![oldest.remote_file_id, newest.remote_file_id]);
    }

    #[test]
    fn list_by_status_orders_by_updated_at() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let now = chrono::Utc::now();
        let mut older = rec("a");
        older.status = SyncStatus::LocalDirty;
        older.updated_at = now - chrono::Duration::seconds(60);
        let mut newer = rec("b");
        newer.status = SyncStatus::LocalDirty;
        newer.updated_at = now;
        let mut other = rec("c");
        other.status = SyncStatus::UpToDate;
        cat.upsert(&older).unwrap();
        cat.upsert(&newer).unwrap();
        cat.upsert(&other).unwrap();
        let queue = cat
            .list_by_status(&[SyncStatus::LocalDirty, SyncStatus::LocalDeleted])
            .unwrap();
        let ids: Vec<_> = queue.iter().map(|r| r.remote_file_id).collect();
        // Oldest-first so the uploader doesn't starve old edits.
        assert_eq!(ids, vec![older.remote_file_id, newer.remote_file_id]);
    }

    #[test]
    fn count_by_status_and_count_pinned_are_independent() {
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        let mut a = rec("a");
        a.pinned = true;
        a.status = SyncStatus::UpToDate;
        let mut b = rec("b");
        b.pinned = true;
        b.status = SyncStatus::LocalDirty;
        let mut c = rec("c");
        c.status = SyncStatus::Evicted;
        cat.upsert(&a).unwrap();
        cat.upsert(&b).unwrap();
        cat.upsert(&c).unwrap();
        assert_eq!(cat.count_pinned().unwrap(), 2);
        assert_eq!(cat.count_by_status(SyncStatus::UpToDate).unwrap(), 1);
        assert_eq!(cat.count_by_status(SyncStatus::LocalDirty).unwrap(), 1);
        assert_eq!(cat.count_by_status(SyncStatus::Evicted).unwrap(), 1);
        assert_eq!(cat.count_by_status(SyncStatus::Conflict).unwrap(), 0);
    }

    #[test]
    fn status_aggregate_matches_individual_counts() {
        // Cross-check: the single-query aggregate MUST produce the
        // same numbers as the legacy 10-query path. If a future
        // refactor changes the CASE filter for `cached_bytes` here
        // but not in `total_cached_bytes` (or vice versa), this
        // test will flag the drift before it ships.
        let tmp = tempfile::tempdir().unwrap();
        let mut cat = Catalogue::open(tmp.path().join("c.db"), Uuid::new_v4()).unwrap();
        // Mix every status and a couple of pinned rows; sizes
        // chosen so the cached_bytes filter is exercised non-trivially.
        let mut up = rec("u");
        up.status = SyncStatus::UpToDate;
        up.size_bytes = 100;
        up.pinned = true;
        let mut ld = rec("ld");
        ld.status = SyncStatus::LocalDirty;
        ld.size_bytes = 50;
        let mut rd = rec("rd");
        rd.status = SyncStatus::RemoteDirty;
        rd.size_bytes = 75;
        let mut cf = rec("cf");
        cf.status = SyncStatus::Conflict;
        cf.size_bytes = 25;
        let mut ev = rec("ev");
        ev.status = SyncStatus::Evicted;
        ev.size_bytes = 999; // excluded from cached_bytes
        let mut deld = rec("dl");
        deld.status = SyncStatus::LocalDeleted;
        deld.size_bytes = 999; // excluded
        let mut delr = rec("dr");
        delr.status = SyncStatus::RemoteDeleted;
        delr.size_bytes = 999; // excluded
        let mut inf = rec("in");
        inf.status = SyncStatus::InFlight;
        inf.size_bytes = 999; // excluded
        inf.pinned = true;
        for r in [&up, &ld, &rd, &cf, &ev, &deld, &delr, &inf] {
            cat.upsert(r).unwrap();
        }
        let (cached, pinned, counts) = cat.status_aggregate().unwrap();
        // Cross-check against the per-query helpers.
        assert_eq!(cached, cat.total_cached_bytes().unwrap());
        assert_eq!(pinned, cat.count_pinned().unwrap());
        assert_eq!(
            counts.up_to_date,
            cat.count_by_status(SyncStatus::UpToDate).unwrap()
        );
        assert_eq!(
            counts.local_dirty,
            cat.count_by_status(SyncStatus::LocalDirty).unwrap()
        );
        assert_eq!(
            counts.local_deleted,
            cat.count_by_status(SyncStatus::LocalDeleted).unwrap()
        );
        assert_eq!(
            counts.remote_dirty,
            cat.count_by_status(SyncStatus::RemoteDirty).unwrap()
        );
        assert_eq!(
            counts.remote_deleted,
            cat.count_by_status(SyncStatus::RemoteDeleted).unwrap()
        );
        assert_eq!(
            counts.conflict,
            cat.count_by_status(SyncStatus::Conflict).unwrap()
        );
        assert_eq!(
            counts.in_flight,
            cat.count_by_status(SyncStatus::InFlight).unwrap()
        );
        assert_eq!(
            counts.evicted,
            cat.count_by_status(SyncStatus::Evicted).unwrap()
        );
        // Explicit numeric check so a regression in EITHER side
        // doesn't silently pass (both could drift in lockstep
        // otherwise).
        assert_eq!(cached, 100 + 50 + 75 + 25);
        assert_eq!(pinned, 2);
        assert_eq!(counts.total(), 8);
    }

    #[test]
    fn migration_heals_empty_last_accessed_at_rows() {
        // Regression for the non-atomic-migration crash hole. Even
        // though the current ALTER + UPDATE run inside one
        // transaction (so SQLite gives us atomicity going forward),
        // a previous build that shipped the un-transacted code
        // could have left rows with `last_accessed_at = ''` on
        // disk. The crash-recovery sweep in `Catalogue::open` MUST
        // backfill those rows from `updated_at` on every open, not
        // just on the open that performs the ALTER.
        let tmp = tempfile::tempdir().unwrap();
        let path = tmp.path().join("c.db");
        let ws = Uuid::new_v4();
        // First open: runs the migration cleanly. Insert one
        // healthy row.
        {
            let mut cat = Catalogue::open(&path, ws).unwrap();
            let r = rec("healthy");
            cat.upsert(&r).unwrap();
        }
        // Simulate a row left over from a pre-transaction build
        // by directly setting `last_accessed_at = ''` on the
        // healthy row. This is exactly the state a crashed
        // migration would leave behind.
        {
            let conn = rusqlite::Connection::open(&path).unwrap();
            let n = conn
                .execute("UPDATE files SET last_accessed_at = ''", [])
                .unwrap();
            assert!(n > 0);
            // Verify the corruption is real before testing the
            // self-heal: a SELECT must fail to parse the row.
            let mut stmt = conn
                .prepare("SELECT last_accessed_at FROM files LIMIT 1")
                .unwrap();
            let s: String = stmt.query_row([], |r| r.get(0)).unwrap();
            assert_eq!(s, "");
        }
        // Re-open: the crash-recovery sweep must populate the
        // empty column from `updated_at`. After re-open, every
        // get() call must succeed (RFC3339 parsing of `''` would
        // fail otherwise).
        {
            let cat = Catalogue::open(&path, ws).unwrap();
            let conn = rusqlite::Connection::open(&path).unwrap();
            let mut stmt = conn.prepare("SELECT last_accessed_at FROM files").unwrap();
            let rows: Vec<String> = stmt
                .query_map([], |r| r.get::<_, String>(0))
                .unwrap()
                .map(|x| x.unwrap())
                .collect();
            assert!(!rows.is_empty());
            for s in &rows {
                assert!(
                    !s.is_empty(),
                    "every row must have a non-empty last_accessed_at after re-open"
                );
                // Parse must succeed; the sweep populates with a
                // value from `updated_at` which itself is RFC3339.
                chrono::DateTime::parse_from_rfc3339(s)
                    .expect("backfilled last_accessed_at must be valid RFC3339");
            }
            // And the high-level catalogue API still returns rows
            // (the corruption case used to make row_to_record
            // return an error).
            let total = cat.total_cached_bytes().unwrap();
            assert!(total > 0);
        }
    }
}
