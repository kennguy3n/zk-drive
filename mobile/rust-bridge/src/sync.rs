//! Local catalogue + changefeed sync surface.
//!
//! Wraps [`zk_sync_engine::Catalogue`] (a SQLite-backed mirror of the
//! user's file tree and its per-file sync state) plus the changefeed
//! catch-up feed so each native platform can implement background sync:
//!
//!   * **Keep a local catalogue** of the file tree — [`upsert`](SyncEngine::upsert),
//!     [`get`](SyncEngine::get), [`list_all`](SyncEngine::list_all),
//!     [`set_status`](SyncEngine::set_status).
//!   * **Poll the changefeed** from a durable cursor — [`poll_once`](SyncEngine::poll_once)
//!     (one wake of an iOS `BGAppRefreshTask` / Android `WorkManager` job),
//!     or [`start`](SyncEngine::start) for a continuous foreground loop
//!     that calls back into a foreign [`ChangeObserver`].
//!   * **Queue transfers that finish when backgrounded** — the catalogue's
//!     [`SyncStatus`] models pending work; [`pending_uploads`](SyncEngine::pending_uploads)
//!     / [`pending_downloads`](SyncEngine::pending_downloads) surface the
//!     queues the platform drains with [`ApiClient`] presigned URLs.
//!
//! # Cursor durability & at-least-once
//!
//! The poll cursor lives in the catalogue's SQLite (`get_cursor` /
//! `set_cursor`), so it survives process death. [`poll_once`] advances
//! the cursor only after it returns the page to the caller's stack
//! frame, which means a crash between "page returned" and "platform
//! persisted the changes" re-delivers that page on the next wake.
//! Changefeed application MUST therefore be idempotent on `sequence`
//! (it already is: catalogue `upsert` is keyed by `remote_file_id`).
//! Platforms needing strict apply-then-commit can instead use
//! [`fetch_changes`](SyncEngine::fetch_changes) + [`commit_cursor`](SyncEngine::commit_cursor).

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use chrono::{TimeZone, Utc};
use tokio::task::JoinHandle;
use uuid::Uuid;
use zk_sync_engine::{Catalogue, FileRecord, SyncStatus as CoreStatus};

use crate::api::{ApiClient, ChangePage};
use crate::error::{BridgeError, Result};
use crate::runtime::spawn;

/// FFI mirror of [`zk_sync_engine::SyncStatus`]. Kept as a standalone
/// enum (rather than reusing the core type) so the FFI contract is
/// owned by the bridge and a future core-only status can be added
/// without breaking generated bindings until the bridge opts in.
#[derive(Debug, Clone, Copy, PartialEq, Eq, uniffi::Enum)]
pub enum SyncStatus {
    UpToDate,
    LocalDirty,
    LocalDeleted,
    RemoteDirty,
    RemoteDeleted,
    Conflict,
    InFlight,
    Evicted,
}

impl From<CoreStatus> for SyncStatus {
    fn from(s: CoreStatus) -> Self {
        match s {
            CoreStatus::UpToDate => SyncStatus::UpToDate,
            CoreStatus::LocalDirty => SyncStatus::LocalDirty,
            CoreStatus::LocalDeleted => SyncStatus::LocalDeleted,
            CoreStatus::RemoteDirty => SyncStatus::RemoteDirty,
            CoreStatus::RemoteDeleted => SyncStatus::RemoteDeleted,
            CoreStatus::Conflict => SyncStatus::Conflict,
            CoreStatus::InFlight => SyncStatus::InFlight,
            CoreStatus::Evicted => SyncStatus::Evicted,
        }
    }
}

impl From<SyncStatus> for CoreStatus {
    fn from(s: SyncStatus) -> Self {
        match s {
            SyncStatus::UpToDate => CoreStatus::UpToDate,
            SyncStatus::LocalDirty => CoreStatus::LocalDirty,
            SyncStatus::LocalDeleted => CoreStatus::LocalDeleted,
            SyncStatus::RemoteDirty => CoreStatus::RemoteDirty,
            SyncStatus::RemoteDeleted => CoreStatus::RemoteDeleted,
            SyncStatus::Conflict => CoreStatus::Conflict,
            SyncStatus::InFlight => CoreStatus::InFlight,
            SyncStatus::Evicted => CoreStatus::Evicted,
        }
    }
}

/// FFI mirror of one catalogue row ([`zk_sync_engine::FileRecord`]) with
/// FFI-friendly field types.
#[derive(Debug, Clone, uniffi::Record)]
pub struct FileEntry {
    pub remote_file_id: String,
    pub remote_version_id: String,
    /// Absolute (or workspace-root-relative) local path the platform
    /// chose for this file.
    pub local_path: String,
    pub size_bytes: u64,
    /// Lowercase hex of the 32-byte content hash. Empty string is
    /// rejected on upsert; pass the all-zero hash hex for a not-yet-
    /// hashed placeholder.
    pub content_hash_hex: String,
    pub status: SyncStatus,
    /// Pinned files are exempt from the offline-cache LRU eviction.
    pub pinned: bool,
    pub updated_at_unix_ms: i64,
}

impl FileEntry {
    fn from_record(r: &FileRecord) -> Self {
        Self {
            remote_file_id: r.remote_file_id.to_string(),
            remote_version_id: r.remote_version_id.to_string(),
            local_path: r.local_path.to_string_lossy().into_owned(),
            size_bytes: r.size_bytes,
            content_hash_hex: hex_encode(&r.content_hash),
            status: r.status.into(),
            pinned: r.pinned,
            updated_at_unix_ms: r.updated_at.timestamp_millis(),
        }
    }

    fn into_record(self) -> Result<FileRecord> {
        let remote_file_id = parse_uuid("remote_file_id", &self.remote_file_id)?;
        let remote_version_id = parse_uuid("remote_version_id", &self.remote_version_id)?;
        if self.local_path.is_empty() {
            return Err(BridgeError::InvalidInput("local_path is empty".into()));
        }
        let content_hash = hex_decode_32(&self.content_hash_hex)?;
        let updated_at = Utc
            .timestamp_millis_opt(self.updated_at_unix_ms)
            .single()
            .ok_or_else(|| {
                BridgeError::InvalidInput(format!(
                    "updated_at_unix_ms {} is not representable",
                    self.updated_at_unix_ms
                ))
            })?;
        Ok(FileRecord {
            remote_file_id,
            remote_version_id,
            local_path: PathBuf::from(self.local_path),
            size_bytes: self.size_bytes,
            content_hash,
            status: self.status.into(),
            pinned: self.pinned,
            updated_at,
        })
    }
}

/// Foreign-implemented sink for the continuous foreground poll loop
/// started by [`SyncEngine::start`]. Both methods are invoked from a
/// background runtime thread, never the platform UI thread.
#[uniffi::export(with_foreign)]
pub trait ChangeObserver: Send + Sync {
    /// A non-empty changefeed page arrived. The platform applies the
    /// mutations to its catalogue / UI. After this returns, the loop
    /// advances the durable cursor past `page.cursor`.
    fn on_changes(&self, page: ChangePage);
    /// A poll attempt failed (network blip, auth expiry). The loop logs
    /// it via the observer and retries on the next interval; the cursor
    /// is left untouched so no changes are skipped.
    fn on_error(&self, message: String);
}

/// Local catalogue + changefeed poller for one workspace.
#[derive(uniffi::Object)]
pub struct SyncEngine {
    workspace_id: Uuid,
    catalogue: Mutex<Catalogue>,
    api: Arc<ApiClient>,
    running: AtomicBool,
    poll_task: Mutex<Option<JoinHandle<()>>>,
}

#[uniffi::export]
impl SyncEngine {
    /// Open (creating if absent) the SQLite catalogue at
    /// `catalogue_path` bound to `workspace_id`, polling changes through
    /// `api`. The path should live in the platform's app-support /
    /// files directory so it persists across launches and is excluded
    /// from cloud backup if the platform requires it.
    #[uniffi::constructor]
    pub fn new(
        catalogue_path: String,
        workspace_id: String,
        api: Arc<ApiClient>,
    ) -> Result<Arc<Self>> {
        let workspace_id = parse_uuid("workspace_id", &workspace_id)?;
        if catalogue_path.is_empty() {
            return Err(BridgeError::InvalidInput("catalogue_path is empty".into()));
        }
        let catalogue = Catalogue::open(&catalogue_path, workspace_id).map_err(BridgeError::from)?;
        Ok(Arc::new(Self {
            workspace_id,
            catalogue: Mutex::new(catalogue),
            api,
            running: AtomicBool::new(false),
            poll_task: Mutex::new(None),
        }))
    }

    /// The workspace this engine is bound to.
    pub fn workspace_id(&self) -> String {
        self.workspace_id.to_string()
    }

    /// Durable poll cursor (highest applied changefeed sequence).
    pub fn cursor(&self) -> Result<i64> {
        let cat = self.lock_catalogue()?;
        cat.get_cursor(self.workspace_id).map_err(BridgeError::from)
    }

    /// Overwrite the durable poll cursor. Use [`commit_cursor`](Self::commit_cursor)
    /// in the normal apply loop; this is for explicit resync / reset.
    pub fn set_cursor(&self, cursor: i64) -> Result<()> {
        let mut cat = self.lock_catalogue()?;
        cat.set_cursor(self.workspace_id, cursor)
            .map_err(BridgeError::from)
    }

    /// Persist `cursor` as the new resume point after the platform has
    /// applied a page. Rejects a regressing cursor so an out-of-order
    /// call can't rewind the feed and re-deliver old mutations forever.
    pub fn commit_cursor(&self, cursor: i64) -> Result<()> {
        let mut cat = self.lock_catalogue()?;
        let current = cat.get_cursor(self.workspace_id).map_err(BridgeError::from)?;
        if cursor < current {
            return Err(BridgeError::InvalidInput(format!(
                "cursor {cursor} regresses below current {current}"
            )));
        }
        cat.set_cursor(self.workspace_id, cursor)
            .map_err(BridgeError::from)
    }

    /// Fetch changes after an explicit `since` sequence, without
    /// touching the durable cursor. Mirrors `apiClient.getChanges(since)`
    /// from the bridge spec; use it for ad-hoc queries (e.g. rendering a
    /// recent-activity view) rather than driving sync.
    pub fn get_changes(&self, since: i64, limit: Option<u32>) -> Result<ChangePage> {
        self.api.get_changes(since, limit)
    }

    /// Fetch the next page from the durable cursor WITHOUT advancing it.
    /// The crash-safe primitive: the platform applies the page, persists
    /// its own state, then calls [`commit_cursor`](Self::commit_cursor).
    pub fn fetch_changes(&self, limit: Option<u32>) -> Result<ChangePage> {
        let since = self.cursor()?;
        self.api.get_changes(since, limit)
    }

    /// One background-sync tick: fetch the next page from the durable
    /// cursor and advance the cursor to the page's cursor. Returns the
    /// page so the platform can apply it. Intended for iOS
    /// `BGAppRefreshTask` / Android `WorkManager` one-shot wakes. See the
    /// module docs for the at-least-once contract.
    pub fn poll_once(&self, limit: Option<u32>) -> Result<ChangePage> {
        let since = self.cursor()?;
        let page = self.api.get_changes(since, limit)?;
        if page.cursor > since {
            self.set_cursor(page.cursor)?;
        }
        Ok(page)
    }

    /// Start a continuous foreground poll loop on the shared runtime,
    /// delivering pages to `observer` every `interval_ms` (floored at
    /// 1s to protect the backend). Idempotent: a second call while
    /// already running is a no-op. Pair with [`stop`](Self::stop) when
    /// the app backgrounds.
    pub fn start(
        self: Arc<Self>,
        observer: Arc<dyn ChangeObserver>,
        interval_ms: u64,
        limit: Option<u32>,
    ) {
        // swap returns the previous value; if it was already true we're
        // running and must not spawn a second loop.
        if self.running.swap(true, Ordering::SeqCst) {
            return;
        }
        let engine = self.clone();
        let period = Duration::from_millis(interval_ms.max(1000));
        let handle = spawn(async move {
            let mut tick = tokio::time::interval(period);
            // Skip missed ticks rather than bursting catch-up polls if a
            // single poll runs long.
            tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
            while engine.running.load(Ordering::SeqCst) {
                tick.tick().await;
                if !engine.running.load(Ordering::SeqCst) {
                    break;
                }
                let since = match engine.cursor() {
                    Ok(c) => c,
                    Err(e) => {
                        observer.on_error(e.to_string());
                        continue;
                    }
                };
                match engine.api.changes_async(since, limit).await {
                    Ok(page) if !page.mutations.is_empty() => {
                        let next = page.cursor;
                        observer.on_changes(page);
                        if next > since {
                            if let Err(e) = engine.set_cursor(next) {
                                observer.on_error(e.to_string());
                            }
                        }
                    }
                    Ok(_) => {}
                    Err(e) => observer.on_error(e.to_string()),
                }
            }
        });
        *self.poll_task.lock().expect("poll_task mutex") = Some(handle);
    }

    /// Stop the continuous poll loop started by [`start`](Self::start).
    /// Idempotent. Blocks briefly until the loop's in-flight tick winds
    /// down.
    pub fn stop(&self) {
        self.running.store(false, Ordering::SeqCst);
        let handle = self.poll_task.lock().expect("poll_task mutex").take();
        if let Some(handle) = handle {
            // The loop checks `running` each tick and exits; abort is a
            // backstop so a long in-flight network poll doesn't keep the
            // task alive past stop().
            handle.abort();
        }
    }

    // ---- catalogue surface -------------------------------------------------

    /// Insert or replace a catalogue row for a file the platform is
    /// tracking. Keyed by `remote_file_id`, so re-applying the same
    /// remote change is idempotent.
    pub fn upsert(&self, entry: FileEntry) -> Result<()> {
        let rec = entry.into_record()?;
        let mut cat = self.lock_catalogue()?;
        cat.upsert(&rec).map_err(BridgeError::from)
    }

    /// Look up a catalogue row by remote file id.
    pub fn get(&self, remote_file_id: String) -> Result<Option<FileEntry>> {
        let id = parse_uuid("remote_file_id", &remote_file_id)?;
        let cat = self.lock_catalogue()?;
        let rec = cat.get(id).map_err(BridgeError::from)?;
        Ok(rec.as_ref().map(FileEntry::from_record))
    }

    /// Look up a catalogue row by the local path the platform assigned.
    pub fn by_local_path(&self, local_path: String) -> Result<Option<FileEntry>> {
        let cat = self.lock_catalogue()?;
        let rec = cat
            .by_local_path(&PathBuf::from(local_path))
            .map_err(BridgeError::from)?;
        Ok(rec.as_ref().map(FileEntry::from_record))
    }

    /// Transition a file's sync status (e.g. mark `RemoteDirty` when a
    /// changefeed mutation says the server copy advanced, or `InFlight`
    /// while a transfer runs).
    pub fn set_status(&self, remote_file_id: String, status: SyncStatus) -> Result<()> {
        let id = parse_uuid("remote_file_id", &remote_file_id)?;
        let mut cat = self.lock_catalogue()?;
        cat.set_status(id, status.into()).map_err(BridgeError::from)
    }

    /// Atomically update status + content hash + size after a local edit
    /// is detected, so the catalogue's view of on-disk bytes stays
    /// current for dedup decisions.
    pub fn set_local_state(
        &self,
        remote_file_id: String,
        status: SyncStatus,
        content_hash_hex: String,
        size_bytes: u64,
    ) -> Result<()> {
        let id = parse_uuid("remote_file_id", &remote_file_id)?;
        let hash = hex_decode_32(&content_hash_hex)?;
        let mut cat = self.lock_catalogue()?;
        cat.set_local_state(id, status.into(), hash, size_bytes)
            .map_err(BridgeError::from)
    }

    /// Every catalogue row. For large trees prefer the targeted queries;
    /// this materialises the whole table.
    pub fn list_all(&self) -> Result<Vec<FileEntry>> {
        let cat = self.lock_catalogue()?;
        let recs = cat.list_all().map_err(BridgeError::from)?;
        Ok(recs.iter().map(FileEntry::from_record).collect())
    }

    /// Files with local changes awaiting upload (`LocalDirty` /
    /// `LocalDeleted`). The platform drains this queue by minting an
    /// [`ApiClient::upload_url`] per entry while backgrounded.
    pub fn pending_uploads(&self) -> Result<Vec<FileEntry>> {
        self.filter_status(&[SyncStatus::LocalDirty, SyncStatus::LocalDeleted])
    }

    /// Files with remote changes awaiting download (`RemoteDirty` /
    /// `RemoteDeleted`). The platform drains this queue by minting an
    /// [`ApiClient::download_url`] per entry (or unlinking for deletes).
    pub fn pending_downloads(&self) -> Result<Vec<FileEntry>> {
        self.filter_status(&[SyncStatus::RemoteDirty, SyncStatus::RemoteDeleted])
    }
}

impl SyncEngine {
    fn lock_catalogue(&self) -> Result<std::sync::MutexGuard<'_, Catalogue>> {
        self.catalogue
            .lock()
            .map_err(|_| BridgeError::Catalogue("catalogue mutex poisoned".into()))
    }

    fn filter_status(&self, wanted: &[SyncStatus]) -> Result<Vec<FileEntry>> {
        let cat = self.lock_catalogue()?;
        let recs = cat.list_all().map_err(BridgeError::from)?;
        Ok(recs
            .iter()
            .filter(|r| {
                let s: SyncStatus = r.status.into();
                wanted.contains(&s)
            })
            .map(FileEntry::from_record)
            .collect())
    }
}

impl Drop for SyncEngine {
    fn drop(&mut self) {
        // Ensure the background loop can't outlive the engine.
        self.running.store(false, Ordering::SeqCst);
        if let Ok(mut guard) = self.poll_task.lock() {
            if let Some(handle) = guard.take() {
                handle.abort();
            }
        }
    }
}

fn parse_uuid(field: &str, s: &str) -> Result<Uuid> {
    Uuid::parse_str(s).map_err(|_| BridgeError::InvalidInput(format!("invalid {field}: {s:?}")))
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        out.push(char::from_digit((b >> 4) as u32, 16).unwrap());
        out.push(char::from_digit((b & 0x0f) as u32, 16).unwrap());
    }
    out
}

fn hex_decode_32(s: &str) -> Result<[u8; 32]> {
    let bytes = s.as_bytes();
    if bytes.len() != 64 {
        return Err(BridgeError::InvalidInput(format!(
            "content_hash_hex must be 64 hex chars (32 bytes), got {}",
            bytes.len()
        )));
    }
    let mut out = [0u8; 32];
    for (i, chunk) in bytes.chunks_exact(2).enumerate() {
        let hi = hex_val(chunk[0])?;
        let lo = hex_val(chunk[1])?;
        out[i] = (hi << 4) | lo;
    }
    Ok(out)
}

fn hex_val(c: u8) -> Result<u8> {
    match c {
        b'0'..=b'9' => Ok(c - b'0'),
        b'a'..=b'f' => Ok(c - b'a' + 10),
        b'A'..=b'F' => Ok(c - b'A' + 10),
        _ => Err(BridgeError::InvalidInput(format!(
            "invalid hex char: {:?}",
            c as char
        ))),
    }
}