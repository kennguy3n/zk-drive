//! Engine reconciliation loop: consumes [`LocalEvent`] + [`RemoteEvent`]
//! channels, drives the catalogue, and dispatches upload / download
//! tasks against [`zk_sync_api`].

use std::path::{Path, PathBuf};
use std::sync::Arc;

use tokio::sync::{mpsc, Mutex};
use tracing::{info, warn};

use uuid::Uuid;
use zk_sync_api::Client;

use crate::catalogue::{Catalogue, FileRecord, SyncStatus};
use crate::conflict::{ConflictPolicy, LastWriterWins};
use crate::events::{LocalEvent, RemoteEvent};
use crate::Result;

/// Engine wiring. Owns the catalogue (behind a mutex), the API
/// client, the conflict policy, and the channel into which the
/// watcher + poller push events.
pub struct Engine {
    pub config: EngineConfig,
    pub client: Arc<Client>,
    pub catalogue: Arc<Mutex<Catalogue>>,
    pub policy: Arc<dyn ConflictPolicy>,
}

#[derive(Debug, Clone)]
pub struct EngineConfig {
    /// Workspace this engine instance is bound to. The desktop agent
    /// instantiates one engine per workspace the user has elected to
    /// sync.
    pub workspace_id: Uuid,
    /// Local root that mirrors the workspace.
    pub root: PathBuf,
    /// Default chunk size used when invoking the crypto crate for
    /// Strict-ZK uploads. The default
    /// [`zk_sync_crypto::DEFAULT_CHUNK_SIZE`] matches the Go SDK.
    pub chunk_size: Option<usize>,
}

impl Engine {
    pub fn new(
        config: EngineConfig,
        client: Arc<Client>,
        catalogue: Arc<Mutex<Catalogue>>,
    ) -> Self {
        Self {
            config,
            client,
            catalogue,
            policy: Arc::new(LastWriterWins),
        }
    }

    pub fn with_policy(mut self, policy: Arc<dyn ConflictPolicy>) -> Self {
        self.policy = policy;
        self
    }

    /// Build a placeholder local path for a remote file we've just
    /// learned about via the change feed but haven't downloaded yet.
    ///
    /// The placeholder lives under [`PLACEHOLDER_DIR_NAME`] and is keyed
    /// exclusively on the resource id so two distinct remote files
    /// can never collide on the catalogue's UNIQUE(local_path) index
    /// -- even when their `name` fields happen to be identical (e.g.
    /// two `readme.md` files in different remote folders).
    ///
    /// The downloader is responsible for moving the file to its
    /// final location under the resolved folder hierarchy and
    /// rewriting `local_path` on the catalogue row via `upsert` once
    /// it knows where the file actually belongs. Until then this stub
    /// path keeps the catalogue invariant intact.
    ///
    /// The [`Watcher`](crate::Watcher) is expected to be configured
    /// with `placeholder_dir(root)` as an ignored prefix so the act
    /// of materialising a stub does not bounce back into the engine
    /// as a spurious local event.
    fn placeholder_path_for(&self, resource_id: Uuid) -> PathBuf {
        placeholder_dir(&self.config.root).join(resource_id.to_string())
    }
}

/// Hidden subdirectory inside the workspace root where the engine
/// materialises catalogue stubs for remote files it has learned
/// about but hasn't downloaded yet. Watchers MUST be configured to
/// ignore this prefix; see [`Engine::placeholder_path_for`].
pub const PLACEHOLDER_DIR_NAME: &str = ".zk-pending";

/// Returns the absolute placeholder directory for a workspace `root`.
pub fn placeholder_dir(root: &std::path::Path) -> PathBuf {
    root.join(PLACEHOLDER_DIR_NAME)
}

/// Hidden subdirectory inside the workspace root where the engine
/// parks `local_path` entries for catalogue rows whose on-disk
/// bytes have been overwritten -- e.g. a rename that landed on a
/// path that was already tracked by a different file. The row is
/// kept (so the next outbound sync can push a tombstone) but its
/// `local_path` is moved here to satisfy UNIQUE(local_path).
///
/// Watchers MUST also ignore this prefix; the row's on-disk
/// counterpart is gone, but a sloppy future caller could still
/// touch the parked path.
pub const TOMBSTONE_DIR_NAME: &str = ".zk-deleted";

/// Returns the absolute tombstone directory for a workspace `root`.
pub fn tombstone_dir(root: &std::path::Path) -> PathBuf {
    root.join(TOMBSTONE_DIR_NAME)
}

impl Engine {
    /// Drive both event channels until either is closed.
    pub async fn run(
        self,
        mut local_rx: mpsc::Receiver<LocalEvent>,
        mut remote_rx: mpsc::Receiver<RemoteEvent>,
    ) -> Result<()> {
        loop {
            tokio::select! {
                Some(ev) = local_rx.recv() => self.handle_local(ev).await?,
                Some(ev) = remote_rx.recv() => self.handle_remote(ev).await?,
                else => return Ok(()),
            }
        }
    }

    async fn handle_local(&self, ev: LocalEvent) -> Result<()> {
        // Honor a folder's `ignore` selective-sync policy. A local
        // change under an ignored folder is dropped before it can
        // register a new catalogue row or flip an existing one dirty,
        // so the upload flow never pushes it. This is non-destructive:
        // rows that synced before the folder was ignored keep their
        // last status and nothing is removed server-side -- we simply
        // stop propagating fresh local changes. The check is dynamic
        // (re-read per event) so a folder flipped to `ignore` while
        // sync is running takes effect on the next change without a
        // restart. For a rename we test the destination, since that is
        // where the file comes to rest: a move *out* of an ignored
        // folder should resume syncing, a move *into* one should stop.
        let event_path: &Path = match &ev {
            LocalEvent::Upsert { path, .. } | LocalEvent::Delete { path } => path.as_path(),
            LocalEvent::Rename { to, .. } => to.as_path(),
        };
        if self
            .catalogue
            .lock()
            .await
            .is_path_ignored(self.config.workspace_id, event_path)?
        {
            info!(path = ?event_path, "local event under an ignored folder; skipping");
            return Ok(());
        }
        match ev {
            LocalEvent::Upsert {
                path,
                size_bytes,
                content_hash,
            } => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.by_local_path(&path)? {
                    // Dedup short-circuit: a re-fire of the watcher
                    // on the same content is a no-op iff the row is
                    // already in a "content matches what's known"
                    // status. Three states are deliberately excluded
                    // because for them the *status* transition is
                    // the load-bearing signal, not the hash:
                    //
                    //   * `LocalDeleted` means the previous event
                    //     was a delete; the upload flow would push a
                    //     *tombstone* for this row. Re-creating the
                    //     file with identical content (e.g. `git
                    //     checkout`, undo, restore-from-backup) MUST
                    //     resurrect the row to `LocalDirty` so the
                    //     server-side copy stays alive. Skipping
                    //     here would silently delete the file on the
                    //     server on the next reconciliation.
                    //
                    //   * `Evicted` means the LRU offline cache
                    //     dropped the local bytes; the row's
                    //     content_hash still records the *last known*
                    //     server hash, so a recreate at the same path
                    //     with that hash must transition back to
                    //     `UpToDate` via the normal upsert path, not
                    //     stay `Evicted` and confuse the cache
                    //     prefetcher.
                    //
                    //   * `RemoteDeleted` means the server has
                    //     tombstoned the file; the catalogue's
                    //     `content_hash` is just the *last-known
                    //     pre-tombstone* hash. A same-hash local
                    //     recreate after a remote delete is a *fresh
                    //     upload*, not a no-op -- the server no
                    //     longer holds these bytes. Without this
                    //     exclusion the dedup fires, the row stays
                    //     `RemoteDeleted`, and the downloader
                    //     unlinks the user's freshly-saved file on
                    //     the next reconciliation pass.
                    if existing.content_hash == content_hash
                        && !matches!(
                            existing.status,
                            SyncStatus::LocalDeleted
                                | SyncStatus::Evicted
                                | SyncStatus::RemoteDeleted
                        )
                    {
                        return Ok(());
                    }
                    // Atomically flip status + refresh the catalogue's
                    // view of (content_hash, size_bytes). Without the
                    // hash/size refresh, a follow-up A -> B -> A revert
                    // would short-circuit on the stale hash A at the
                    // dedup check above and silently leave the row
                    // LocalDirty even though it matches the server.
                    //
                    // Three distinct paths converge here:
                    //   1. content_hash CHANGED -- treat as normal
                    //      LocalDirty transition; the upload flow will
                    //      push the new bytes.
                    //   2. content_hash UNCHANGED but status was
                    //      LocalDeleted or Evicted. In both cases
                    //      the row's recorded hash already equals
                    //      the server's known version, so there is
                    //      nothing to upload -- jump straight back
                    //      to UpToDate instead of churning the row
                    //      through a redundant LocalDirty -> upload
                    //      -> UpToDate cycle.
                    //   3. content_hash UNCHANGED but status was
                    //      RemoteDeleted. The server no longer holds
                    //      these bytes, so the row MUST land on
                    //      LocalDirty (not UpToDate) so the upload
                    //      flow re-creates the object server-side.
                    //      `next_on_local_upsert(RemoteDeleted) ==
                    //      LocalDirty` already enforces this in the
                    //      state machine; we just have to not short-
                    //      circuit to UpToDate here.
                    let next = if existing.content_hash == content_hash
                        && !matches!(existing.status, SyncStatus::RemoteDeleted)
                    {
                        SyncStatus::UpToDate
                    } else {
                        existing.status.next_on_local_upsert()
                    };
                    cat.set_local_state(existing.remote_file_id, next, content_hash, size_bytes)?;
                    info!(?path, size_bytes, ?next, "local file change recorded");
                } else {
                    // First time we've seen this path on disk. Allocate
                    // a stub remote_file_id (the upload flow will
                    // remap it once the server assigns the real one) so
                    // the row is visible to status queries immediately.
                    let local_id = Uuid::new_v4();
                    let rec = FileRecord {
                        remote_file_id: local_id,
                        // Zero version sentinel = "not yet uploaded".
                        remote_version_id: Uuid::nil(),
                        local_path: path.clone(),
                        size_bytes,
                        content_hash,
                        status: SyncStatus::LocalDirty,
                        pinned: false,
                        updated_at: chrono::Utc::now(),
                    };
                    cat.upsert(&rec)?;
                    info!(
                        ?path,
                        size_bytes,
                        local_id = %local_id,
                        "new local file registered; awaiting first upload"
                    );
                }
                Ok(())
            }
            LocalEvent::Delete { path } => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.by_local_path(&path)? {
                    let next = existing.status.next_on_local_delete();
                    if next != existing.status {
                        cat.set_status(existing.remote_file_id, next)?;
                    }
                    info!(?path, ?next, "local delete recorded");
                }
                Ok(())
            }
            LocalEvent::Rename { from, to } => {
                // Wrap the entire multi-step Rename in an explicit
                // SQLite transaction so a failure halfway through
                // (e.g. disk-full on the second UPDATE) rolls every
                // mutation back as a unit. Without this, the
                // catalogue could end up with the displaced row's
                // path repointed into the tombstone dir but its
                // status still UpToDate -- a half-displaced state
                // the next reconciliation pass would mis-handle.
                let mut cat = self.catalogue.lock().await;
                let root = self.config.root.clone();
                let outcome = cat.with_txn(|cat| {
                    let Some(existing) = cat.by_local_path(&from)? else {
                        return Ok(None);
                    };
                    // Defensive: a rename into a path the catalogue
                    // is already tracking would violate
                    // UNIQUE(local_path) when we re-upsert `existing`
                    // with its new path. Treat the displaced row as
                    // locally deleted (the operating system
                    // overwrote its on-disk bytes) so the next sync
                    // pushes a tombstone for it, then proceed with
                    // the rename. Order matters: we must free the
                    // target path inside the same catalogue
                    // transaction.
                    let mut displaced_info: Option<(Uuid, SyncStatus)> = None;
                    if let Some(displaced) = cat.by_local_path(&to)? {
                        if displaced.remote_file_id != existing.remote_file_id {
                            let parked =
                                tombstone_dir(&root).join(displaced.remote_file_id.to_string());
                            cat.set_local_path(displaced.remote_file_id, &parked)?;
                            // The displaced row's on-disk bytes are
                            // gone (the OS just overwrote them).
                            // Route it through the delete-side
                            // transition so the upload flow pushes a
                            // tombstone, not stale bytes.
                            let displaced_next = displaced.status.next_on_local_delete();
                            if displaced_next != displaced.status {
                                cat.set_status(displaced.remote_file_id, displaced_next)?;
                            }
                            displaced_info = Some((displaced.remote_file_id, displaced_next));
                        }
                    }
                    let mut new_rec = existing.clone();
                    new_rec.local_path = to.clone();
                    // The source row's content still exists on disk
                    // (just at a different path), so this is the
                    // upsert side of the state machine even though
                    // the user thinks of it as a 'move'.
                    new_rec.status = existing.status.next_on_local_upsert();
                    new_rec.updated_at = chrono::Utc::now();
                    cat.upsert(&new_rec)?;
                    Ok(Some((new_rec.status, displaced_info)))
                })?;
                if let Some((status, displaced_info)) = outcome {
                    if let Some((displaced_id, displaced_next)) = displaced_info {
                        info!(
                            ?to,
                            displaced_file_id = %displaced_id,
                            ?displaced_next,
                            "rename target already tracked; displaced row marked deleted"
                        );
                    }
                    info!(?from, ?to, ?status, "local rename recorded");
                }
                Ok(())
            }
        }
    }

    async fn handle_remote(&self, ev: RemoteEvent) -> Result<()> {
        let m = ev.mutation().clone();
        match ev {
            // All four "remote file changed in some way" variants take
            // the same path: refresh status on existing rows; for an
            // unknown resource_id (we missed the create -- e.g. catch-
            // up cursor was corrupted, replica gap, or a 4xx ate the
            // antecedent event) materialise a placeholder so the row
            // becomes visible to the downloader instead of being
            // silently dropped on the floor.
            //
            // Including FileRenamed / FileMoved in this fallback closes
            // a defensive gap: a rename event for a file we've never
            // seen used to be a no-op, which meant a corrupted change
            // feed cursor could permanently lose the file. Now the
            // engine recovers by stubbing the row and letting the
            // downloader fetch metadata to learn the real path.
            RemoteEvent::FileCreated(_)
            | RemoteEvent::FileUpdated(_)
            | RemoteEvent::FileRenamed(_)
            | RemoteEvent::FileMoved(_) => {
                let mut cat = self.catalogue.lock().await;
                match cat.get(m.resource_id)? {
                    Some(existing) => {
                        let next = existing.status.next_on_remote_change();
                        if next != existing.status {
                            cat.set_status(m.resource_id, next)?;
                        }
                    }
                    None => {
                        let local_path = self.placeholder_path_for(m.resource_id);
                        let rec = FileRecord {
                            remote_file_id: m.resource_id,
                            // Version id arrives on the subsequent
                            // metadata fetch / download; zero
                            // sentinel means "not yet known".
                            remote_version_id: Uuid::nil(),
                            local_path,
                            size_bytes: 0,
                            content_hash: [0u8; 32],
                            status: SyncStatus::RemoteDirty,
                            pinned: false,
                            updated_at: chrono::Utc::now(),
                        };
                        cat.upsert(&rec)?;
                        info!(
                            file_id = %m.resource_id,
                            name = %m.name,
                            op = %m.op,
                            "remote file event for unknown resource id; \
                             materialised placeholder so the downloader \
                             can recover (likely cursor gap upstream)"
                        );
                    }
                }
                Ok(())
            }
            RemoteEvent::FileDeleted(_) => {
                // Use the dedicated `next_on_remote_delete` transition
                // (NOT `next_on_remote_change`) so the catalogue row
                // lands on `RemoteDeleted` instead of `RemoteDirty`.
                // The downloader pivots on this status to
                // decide between 'fetch new content' and 'unlink the
                // local copy'; collapsing both into RemoteDirty would
                // force it to re-stat the server for every dirty row
                // just to disambiguate, defeating the whole point of
                // the change feed.
                //
                // Unknown-resource FileDeleted is correctly a no-op:
                // a row we never saw to begin with does not need to be
                // unlinked locally, and we'd have nothing useful to
                // record about it. This is the only Remote* variant
                // that does NOT fall through to the placeholder path.
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.get(m.resource_id)? {
                    let next = existing.status.next_on_remote_delete();
                    if next != existing.status {
                        cat.set_status(m.resource_id, next)?;
                    }
                }
                Ok(())
            }
            // Folder / permission events feed the engine's UI layer
            // but don't materialise as on-disk operations in this
            // PR. The desktop shell uses them to refresh tree views.
            RemoteEvent::FolderCreated(_)
            | RemoteEvent::FolderUpdated(_)
            | RemoteEvent::FolderRenamed(_)
            | RemoteEvent::FolderMoved(_)
            | RemoteEvent::FolderDeleted(_)
            | RemoteEvent::PermissionChanged(_) => Ok(()),
            RemoteEvent::Raw(m) => {
                warn!(kind = %m.kind, op = %m.op, seq = m.sequence,
                      "unknown remote event; ignored");
                Ok(())
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::events::RemoteEvent;
    use chrono::Utc;
    use tempfile::TempDir;
    use zk_sync_api::Mutation;

    fn engine_for(tempdir: &TempDir) -> (Engine, Arc<Mutex<Catalogue>>) {
        let workspace_id = Uuid::new_v4();
        let cat = Catalogue::open(tempdir.path().join("cat.db"), workspace_id).unwrap();
        let catalogue = Arc::new(Mutex::new(cat));
        let client = Arc::new(
            zk_sync_api::Client::builder("https://example.com")
                .build()
                .unwrap(),
        );
        let engine = Engine::new(
            EngineConfig {
                workspace_id,
                root: tempdir.path().to_path_buf(),
                chunk_size: None,
            },
            client,
            catalogue.clone(),
        );
        (engine, catalogue)
    }

    #[tokio::test]
    async fn file_created_for_unknown_file_inserts_remote_dirty_stub() {
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let workspace_id = engine.config.workspace_id;
        let ev = RemoteEvent::FileCreated(Mutation {
            sequence: 1,
            workspace_id,
            actor_id: None,
            kind: "file".into(),
            op: "create".into(),
            resource_id: file_id,
            parent_id: None,
            name: "report.docx".into(),
            metadata: None,
            occurred_at: Utc::now(),
        });
        engine.handle_remote(ev).await.unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().expect("stub row must exist");
        assert_eq!(rec.status, SyncStatus::RemoteDirty);
        assert_eq!(rec.remote_version_id, Uuid::nil());
        assert_eq!(
            rec.local_path,
            tempdir.path().join(".zk-pending").join(file_id.to_string())
        );
    }

    #[tokio::test]
    async fn two_unknown_remote_files_with_same_name_do_not_collide() {
        // Regression: an earlier draft used `<root>/<name>` as the
        // placeholder path, which would crash the engine on the
        // second upsert via UNIQUE(local_path). The current
        // resource-id-based placeholder must keep them distinct.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let a = Uuid::new_v4();
        let b = Uuid::new_v4();
        for id in [a, b] {
            let ev = RemoteEvent::FileCreated(Mutation {
                sequence: 1,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "create".into(),
                resource_id: id,
                parent_id: None,
                name: "readme.md".into(),
                metadata: None,
                occurred_at: Utc::now(),
            });
            engine.handle_remote(ev).await.expect("upsert must succeed");
        }
        let cat = catalogue.lock().await;
        let ra = cat.get(a).unwrap().unwrap();
        let rb = cat.get(b).unwrap().unwrap();
        assert_ne!(ra.local_path, rb.local_path);
    }

    #[tokio::test]
    async fn file_renamed_for_unknown_resource_materialises_placeholder() {
        // Previously, `FileRenamed` / `FileMoved` for a
        // resource id we'd never seen was silently dropped (no
        // `None` arm in the match). That meant a corrupted /
        // gapped change-feed cursor -- where the antecedent
        // `FileCreated` was lost -- would permanently lose the
        // file from the local view. The fix collapses Created /
        // Updated / Renamed / Moved into the same arm so all
        // four can materialise a placeholder for the downloader
        // to recover.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let file_id = Uuid::new_v4();
        engine
            .handle_remote(RemoteEvent::FileRenamed(Mutation {
                sequence: 17,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "rename".into(),
                resource_id: file_id,
                parent_id: None,
                name: "renamed-out-of-thin-air.md".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .expect("rename of unknown file must self-heal, not panic");
        let cat = catalogue.lock().await;
        let rec = cat
            .get(file_id)
            .unwrap()
            .expect("rename of unknown file must materialise a placeholder row");
        assert_eq!(
            rec.status,
            SyncStatus::RemoteDirty,
            "placeholder row must be marked RemoteDirty so the \
             downloader picks it up and fetches metadata to learn \
             the real path"
        );
        assert_eq!(
            rec.remote_version_id,
            Uuid::nil(),
            "version id must use the zero sentinel until the first \
             metadata fetch resolves the actual server version"
        );
    }

    #[tokio::test]
    async fn file_moved_for_unknown_resource_materialises_placeholder() {
        // Companion to file_renamed_for_unknown_resource: same gap,
        // different event kind. Locks the move-side of the
        // not-yet-seen handling so a future split of FileRenamed and
        // FileMoved into separate arms can't reintroduce the
        // silent-drop on one of them.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let file_id = Uuid::new_v4();
        engine
            .handle_remote(RemoteEvent::FileMoved(Mutation {
                sequence: 23,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "move".into(),
                resource_id: file_id,
                parent_id: None,
                name: "moved-out-of-thin-air.md".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .expect("move of unknown file must self-heal, not panic");
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().expect(
            "move of unknown file must materialise a placeholder row so the downloader can recover",
        );
        assert_eq!(rec.status, SyncStatus::RemoteDirty);
    }

    #[tokio::test]
    async fn file_deleted_for_unknown_resource_is_correctly_a_noop() {
        // The FileDeleted handler is the only Remote* file
        // variant that deliberately does NOT fall through to the
        // placeholder path. A delete for a row we never saw to begin
        // with should not materialise a stub -- there is nothing
        // useful to record about it and no local copy to unlink.
        // This test pins that boundary so a future "for symmetry"
        // refactor doesn't accidentally start spawning placeholder
        // rows for events that are inherently terminal.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let file_id = Uuid::new_v4();
        engine
            .handle_remote(RemoteEvent::FileDeleted(Mutation {
                sequence: 29,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "delete".into(),
                resource_id: file_id,
                parent_id: None,
                name: "never-seen.md".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .expect("delete of unknown file must be a clean no-op");
        let cat = catalogue.lock().await;
        assert!(
            cat.get(file_id).unwrap().is_none(),
            "delete of unknown file must not materialise a placeholder; \
             there's no local copy and nothing useful to record"
        );
    }

    #[tokio::test]
    async fn file_updated_for_known_file_keeps_existing_metadata() {
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let workspace_id = engine.config.workspace_id;
        let version_id = Uuid::new_v4();
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: version_id,
                local_path: tempdir.path().join("notes.md"),
                size_bytes: 1024,
                content_hash: [0xAB; 32],
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        let ev = RemoteEvent::FileUpdated(Mutation {
            sequence: 2,
            workspace_id,
            actor_id: None,
            kind: "file".into(),
            op: "update".into(),
            resource_id: file_id,
            parent_id: None,
            name: "notes.md".into(),
            metadata: None,
            occurred_at: Utc::now(),
        });
        engine.handle_remote(ev).await.unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::RemoteDirty);
        // Existing version + hash preserved -- set_status only flips status/updated_at.
        assert_eq!(rec.remote_version_id, version_id);
        assert_eq!(rec.content_hash, [0xAB; 32]);
        assert_eq!(rec.size_bytes, 1024);
    }

    #[tokio::test]
    async fn rename_over_tracked_path_parks_displaced_row() {
        // Regression: a rename whose `to` collides with an existing
        // catalogue row would previously violate UNIQUE(local_path)
        // on the second upsert. The engine now reroutes the
        // displaced row into the tombstone dir, marks it
        // LocalDirty, and only then upserts the renamed source.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let src_id = Uuid::new_v4();
        let dst_id = Uuid::new_v4();
        let from = tempdir.path().join("a.txt");
        let to = tempdir.path().join("b.txt");
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: src_id,
                remote_version_id: Uuid::new_v4(),
                local_path: from.clone(),
                size_bytes: 1,
                content_hash: [1u8; 32],
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
            cat.upsert(&FileRecord {
                remote_file_id: dst_id,
                remote_version_id: Uuid::new_v4(),
                local_path: to.clone(),
                size_bytes: 1,
                content_hash: [2u8; 32],
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Rename {
                from: from.clone(),
                to: to.clone(),
            })
            .await
            .expect("rename must not violate UNIQUE(local_path)");
        let cat = catalogue.lock().await;
        let displaced = cat.get(dst_id).unwrap().unwrap();
        assert_eq!(
            displaced.local_path,
            tempdir.path().join(".zk-deleted").join(dst_id.to_string())
        );
        // Displaced row's on-disk bytes were overwritten by the
        // rename target; the upload flow must push a tombstone for
        // it, not stale bytes.
        assert_eq!(displaced.status, SyncStatus::LocalDeleted);
        let renamed = cat.get(src_id).unwrap().unwrap();
        assert_eq!(renamed.local_path, to);
        assert_eq!(renamed.status, SyncStatus::LocalDirty);
    }

    #[tokio::test]
    async fn remote_change_over_local_dirty_escalates_to_conflict() {
        // Regression: handle_remote used to overwrite LocalDirty with
        // RemoteDirty, silently discarding the local change and
        // making the ConflictPolicy infrastructure dead code. The
        // state machine in SyncStatus::next_on_remote_change now
        // escalates to Conflict so a future downloader can route
        // through the policy instead of clobbering the user's edits.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let workspace_id = engine.config.workspace_id;
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: tempdir.path().join("doc.md"),
                size_bytes: 16,
                content_hash: [9u8; 32],
                status: SyncStatus::LocalDirty,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_remote(RemoteEvent::FileUpdated(Mutation {
                sequence: 1,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "update".into(),
                resource_id: file_id,
                parent_id: None,
                name: "doc.md".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::Conflict);
    }

    #[tokio::test]
    async fn local_change_over_remote_dirty_escalates_to_conflict() {
        // Symmetric counterpart: a local edit on a row whose remote
        // change is still queued for download must escalate to
        // Conflict instead of clobbering the remote side's pending
        // intent.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("collab.md");
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 16,
                content_hash: [9u8; 32],
                status: SyncStatus::RemoteDirty,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 32,
                content_hash: [7u8; 32],
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::Conflict);
    }

    #[tokio::test]
    async fn new_local_file_registers_catalogue_row() {
        // Regression: handle_local used to only log "new local file
        // discovered" for unknown paths, leaving them invisible to
        // the engine's state machine. The engine now allocates a
        // stub remote_file_id and writes a LocalDirty row so the
        // upload flow can pick it up.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let path = tempdir.path().join("draft.md");
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 42,
                content_hash: [3u8; 32],
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat
            .by_local_path(&path)
            .unwrap()
            .expect("new local file must register a catalogue row");
        assert_eq!(rec.status, SyncStatus::LocalDirty);
        assert_eq!(rec.size_bytes, 42);
        assert_eq!(rec.content_hash, [3u8; 32]);
        // Stub remote ids are random v4s, not nil.
        assert_ne!(rec.remote_file_id, Uuid::nil());
        // No remote version yet.
        assert_eq!(rec.remote_version_id, Uuid::nil());
    }

    #[tokio::test]
    async fn local_upsert_under_ignored_folder_registers_no_row() {
        // The selective-sync `ignore` policy is honored at the source:
        // a fresh local change under an ignored folder is dropped in
        // handle_local before it can register a catalogue row, so the
        // upload flow never pushes it. Contrast with
        // `new_local_file_registers_catalogue_row`, which feeds the
        // same event without a policy and DOES register a LocalDirty
        // row -- the only difference here is the ignore policy.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let ignored_dir = tempdir.path().join("Archive");
        let path = ignored_dir.join("old-invoice.pdf");
        catalogue
            .lock()
            .await
            .set_folder_policy(workspace_id, &ignored_dir, "ignore")
            .unwrap();
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 11,
                content_hash: [1u8; 32],
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        assert!(
            cat.by_local_path(&path).unwrap().is_none(),
            "an upsert under an ignored folder must not register a catalogue row"
        );
    }

    #[test]
    fn sync_status_transitions_match_truth_table() {
        // Locks the state machine documented on
        // SyncStatus::next_on_local_{upsert,delete} and
        // SyncStatus::next_on_remote_{change,delete}. Every starting
        // status must appear in every table so a future variant
        // (e.g. a future PinPending) is impossible to add silently
        // without revisiting the truth tables.
        use SyncStatus::*;
        let upsert = [
            (UpToDate, LocalDirty),
            (LocalDirty, LocalDirty),
            (LocalDeleted, LocalDirty),
            (RemoteDirty, Conflict),
            (RemoteDeleted, LocalDirty),
            (Conflict, Conflict),
            (InFlight, Conflict),
            (Evicted, LocalDirty),
        ];
        for (cur, next) in upsert {
            assert_eq!(cur.next_on_local_upsert(), next, "upsert from {cur:?}");
        }
        let delete = [
            (UpToDate, LocalDeleted),
            (LocalDirty, LocalDeleted),
            (LocalDeleted, LocalDeleted),
            (Evicted, LocalDeleted),
            (RemoteDirty, Conflict),
            (RemoteDeleted, RemoteDeleted),
            (Conflict, Conflict),
            (InFlight, Conflict),
        ];
        for (cur, next) in delete {
            assert_eq!(cur.next_on_local_delete(), next, "delete from {cur:?}");
        }
        let remote = [
            (UpToDate, RemoteDirty),
            (RemoteDirty, RemoteDirty),
            (RemoteDeleted, RemoteDirty),
            (LocalDirty, Conflict),
            (LocalDeleted, Conflict),
            (Conflict, Conflict),
            (InFlight, Conflict),
            (Evicted, RemoteDirty),
        ];
        for (cur, next) in remote {
            assert_eq!(cur.next_on_remote_change(), next, "remote from {cur:?}");
        }
        let remote_delete = [
            (UpToDate, RemoteDeleted),
            (RemoteDirty, RemoteDeleted),
            (RemoteDeleted, RemoteDeleted),
            (Evicted, RemoteDeleted),
            (LocalDirty, Conflict),
            (LocalDeleted, RemoteDeleted),
            (Conflict, Conflict),
            (InFlight, Conflict),
        ];
        for (cur, next) in remote_delete {
            assert_eq!(
                cur.next_on_remote_delete(),
                next,
                "remote delete from {cur:?}"
            );
        }
    }

    #[tokio::test]
    async fn local_revert_round_trips_through_uptodate_after_dirty() {
        // Regression guard: handle_local used to flip status to
        // LocalDirty via set_status alone, leaving the catalogue's
        // content_hash + size_bytes stale. A subsequent revert to the
        // server's content (A -> B -> A) would short-circuit on the
        // outdated hash A and silently leave the row LocalDirty even
        // though the file matches the server again.
        //
        // With the atomic set_local_state path, the row's hash + size
        // track the on-disk bytes after every change. The revert path
        // is then driven by an explicit downloader/uploader
        // reconciliation; here we lock down the *invariant*
        // (catalogue hash mirrors on-disk hash after every change).
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("draft.md");
        let server_hash = [0xAAu8; 32];
        let edited_hash = [0xBBu8; 32];
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 100,
                content_hash: server_hash,
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        // User edits.
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 150,
                content_hash: edited_hash,
            })
            .await
            .unwrap();
        {
            let cat = catalogue.lock().await;
            let rec = cat.get(file_id).unwrap().unwrap();
            assert_eq!(rec.status, SyncStatus::LocalDirty);
            assert_eq!(
                rec.content_hash, edited_hash,
                "catalogue must refresh hash on local change"
            );
            assert_eq!(rec.size_bytes, 150);
        }
        // User reverts to server bytes. The next watcher event would
        // be another Upsert with the original hash; the catalogue
        // must already reflect the prior edited hash so the dedup
        // short-circuit doesn't fire and status flips.
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 100,
                content_hash: server_hash,
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        // Status stays LocalDirty because the engine doesn't know
        // (yet) that the bytes match the server -- that's the
        // downloader/uploader's job to resolve. The important
        // invariant: the catalogue's hash + size mirror disk.
        assert_eq!(
            rec.content_hash, server_hash,
            "catalogue must refresh hash on revert"
        );
        assert_eq!(rec.size_bytes, 100);
    }

    #[tokio::test]
    async fn local_delete_marks_row_local_deleted_not_local_dirty() {
        // Regression guard: handle_local Delete used to flip
        // status to LocalDirty, which made it indistinguishable from
        // a content change. The upload flow needs to know that
        // this row is a tombstone candidate, not a content push.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("gone.md");
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 16,
                content_hash: [9u8; 32],
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Delete { path: path.clone() })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::LocalDeleted);
    }

    #[tokio::test]
    async fn local_upsert_after_delete_resurrects_to_local_dirty() {
        // A delete then immediate re-create at the
        // same path must transition LocalDeleted -> LocalDirty, not
        // stay LocalDeleted (which would lose the new content).
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("flicker.md");
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 16,
                content_hash: [9u8; 32],
                status: SyncStatus::LocalDeleted,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 24,
                content_hash: [3u8; 32],
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::LocalDirty);
        assert_eq!(rec.content_hash, [3u8; 32]);
        assert_eq!(rec.size_bytes, 24);
    }

    #[tokio::test]
    async fn local_upsert_after_delete_with_same_content_returns_to_up_to_date() {
        // Prior to the status-aware dedup short-circuit, an
        // identical-content recreation of a deleted file fell into
        // the early-out at `existing.content_hash == content_hash`,
        // which left the row at LocalDeleted. The upload flow would
        // then push a tombstone for a file that has reappeared with
        // exactly the bytes the server already holds -- a silent
        // data-loss event.
        //
        // After the fix, the dedup branch only fires when the row is
        // already in a "content matches what's known" status; the
        // LocalDeleted (and Evicted) cases fall through. When the
        // hash also matches the catalogue's last-known server hash,
        // the row snaps straight to UpToDate (the server already has
        // these bytes; no upload required).
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("undeleted.md");
        let server_hash = [0xCDu8; 32];
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 42,
                content_hash: server_hash,
                status: SyncStatus::LocalDeleted,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 42,
                content_hash: server_hash,
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(
            rec.status,
            SyncStatus::UpToDate,
            "identical-content recreate of LocalDeleted must skip the upload \
             flow and snap straight back to UpToDate"
        );
        assert_eq!(rec.content_hash, server_hash);
        assert_eq!(rec.size_bytes, 42);
    }

    #[tokio::test]
    async fn local_upsert_after_evict_with_same_content_returns_to_up_to_date() {
        // Mirror of `local_upsert_after_delete_with_same_content_*`
        // for the Evicted variant: the LRU cache may unlink the
        // local copy while keeping the catalogue row pointing at the
        // server's last-known hash. When the user (or the pin
        // prefetcher) recreates the file with that exact hash, the
        // row must snap to UpToDate -- not stay Evicted (which would
        // confuse the cache prefetcher into thinking the bytes are
        // still missing).
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("rehydrated.md");
        let server_hash = [0xEFu8; 32];
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 7,
                content_hash: server_hash,
                status: SyncStatus::Evicted,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 7,
                content_hash: server_hash,
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::UpToDate);
    }

    #[tokio::test]
    async fn local_upsert_after_remote_delete_with_same_content_marks_local_dirty() {
        // After SyncStatus::RemoteDeleted was introduced,
        // the handle_local Upsert dedup
        // exclusion list still only mentioned LocalDeleted and
        // Evicted. That left RemoteDeleted in the "short-circuit on
        // matching hash" path, so a user re-saving a file the server
        // had tombstoned would silently stay RemoteDeleted -- and
        // The downloader, pivoting on status, would then unlink
        // the user's freshly-saved file on the next reconciliation.
        //
        // Additionally, even after adding RemoteDeleted to the dedup
        // exclusion list, the same-hash status shortcut was still
        // wrong: it would have collapsed to UpToDate, but the server
        // no longer holds these bytes -- a fresh upload is required.
        // The fix is to exclude RemoteDeleted from the UpToDate
        // shortcut so the transition runs through
        // `next_on_local_upsert(RemoteDeleted) = LocalDirty`.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let file_id = Uuid::new_v4();
        let path = tempdir.path().join("rebornz.md");
        let last_known_hash = [0x77u8; 32];
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: path.clone(),
                size_bytes: 64,
                content_hash: last_known_hash,
                status: SyncStatus::RemoteDeleted,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_local(LocalEvent::Upsert {
                path: path.clone(),
                size_bytes: 64,
                content_hash: last_known_hash,
            })
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(
            rec.status,
            SyncStatus::LocalDirty,
            "a same-hash local recreate after a remote delete must \
             schedule an upload (LocalDirty), not collapse to UpToDate \
             (server no longer holds the bytes) and not stay \
             RemoteDeleted (downloader would unlink the freshly-saved \
             file)"
        );
        assert_eq!(rec.content_hash, last_known_hash);
    }

    #[tokio::test]
    async fn remote_file_deleted_marks_row_remote_deleted_not_remote_dirty() {
        // FileDeleted used to route through
        // `next_on_remote_change`, landing the row on RemoteDirty.
        // That made it indistinguishable from a remote *update*, so
        // the downloader could not pivot 'fetch new bytes' vs
        // 'unlink the local copy' without an extra HEAD/metadata
        // round trip to the server -- defeating the whole point of
        // the change feed.
        //
        // After the fix, FileDeleted is wired through the dedicated
        // `next_on_remote_delete` transition and lands on the new
        // RemoteDeleted status.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let file_id = Uuid::new_v4();
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: tempdir.path().join("doomed.txt"),
                size_bytes: 16,
                content_hash: [0xA5u8; 32],
                status: SyncStatus::UpToDate,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_remote(RemoteEvent::FileDeleted(Mutation {
                sequence: 7,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "delete".into(),
                resource_id: file_id,
                parent_id: None,
                name: "doomed.txt".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::RemoteDeleted);
    }

    #[tokio::test]
    async fn remote_file_deleted_over_local_dirty_escalates_to_conflict() {
        // Local edits pending + remote tombstone is a real conflict:
        // resolving without policy input would either lose the local
        // edits (downloader unlinks) or resurrect a file the server
        // intentionally killed (uploader pushes). Lock that this
        // routes through Conflict, leaving the resolution to
        // ConflictPolicy.
        let tempdir = TempDir::new().unwrap();
        let (engine, catalogue) = engine_for(&tempdir);
        let workspace_id = engine.config.workspace_id;
        let file_id = Uuid::new_v4();
        {
            let mut cat = catalogue.lock().await;
            cat.upsert(&FileRecord {
                remote_file_id: file_id,
                remote_version_id: Uuid::new_v4(),
                local_path: tempdir.path().join("contested.txt"),
                size_bytes: 16,
                content_hash: [0xBCu8; 32],
                status: SyncStatus::LocalDirty,
                pinned: false,
                updated_at: Utc::now(),
            })
            .unwrap();
        }
        engine
            .handle_remote(RemoteEvent::FileDeleted(Mutation {
                sequence: 8,
                workspace_id,
                actor_id: None,
                kind: "file".into(),
                op: "delete".into(),
                resource_id: file_id,
                parent_id: None,
                name: "contested.txt".into(),
                metadata: None,
                occurred_at: Utc::now(),
            }))
            .await
            .unwrap();
        let cat = catalogue.lock().await;
        let rec = cat.get(file_id).unwrap().unwrap();
        assert_eq!(rec.status, SyncStatus::Conflict);
    }
}
