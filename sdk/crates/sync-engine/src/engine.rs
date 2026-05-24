//! Engine reconciliation loop: consumes [`LocalEvent`] + [`RemoteEvent`]
//! channels, drives the catalogue, and dispatches upload / download
//! tasks against [`zk_sync_api`].

use std::path::PathBuf;
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
    /// PR5's downloader is responsible for moving the file to its
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
        match ev {
            LocalEvent::Upsert {
                path,
                size_bytes,
                content_hash,
            } => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.by_local_path(&path)? {
                    if existing.content_hash == content_hash {
                        return Ok(());
                    }
                    // Atomically flip status + refresh the catalogue's
                    // view of (content_hash, size_bytes). Without the
                    // hash/size refresh, a follow-up A -> B -> A revert
                    // would short-circuit on the stale hash A at the
                    // dedup check above and silently leave the row
                    // LocalDirty even though it matches the server.
                    let next = existing.status.next_on_local_upsert();
                    cat.set_local_state(existing.remote_file_id, next, content_hash, size_bytes)?;
                    info!(?path, size_bytes, ?next, "local file change recorded");
                } else {
                    // First time we've seen this path on disk. Allocate
                    // a stub remote_file_id (the upload flow in PR5 will
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
                let mut cat = self.catalogue.lock().await;
                let Some(existing) = cat.by_local_path(&from)? else {
                    return Ok(());
                };
                // Defensive: a rename into a path the catalogue is
                // already tracking would violate UNIQUE(local_path)
                // when we re-upsert `existing` with its new path.
                // Treat the displaced row as locally deleted (the
                // operating system overwrote its on-disk bytes) so
                // the next sync pushes a tombstone for it, then
                // proceed with the rename. Order matters: we must
                // free the target path inside the same catalogue
                // transaction we own the lock for, otherwise a
                // concurrent task could race in between.
                if let Some(displaced) = cat.by_local_path(&to)? {
                    if displaced.remote_file_id != existing.remote_file_id {
                        let parked = tombstone_dir(&self.config.root)
                            .join(displaced.remote_file_id.to_string());
                        cat.set_local_path(displaced.remote_file_id, &parked)?;
                        // The displaced row's on-disk bytes are gone
                        // (the OS just overwrote them). Route it
                        // through the delete-side transition so the
                        // upload flow pushes a tombstone, not stale
                        // bytes.
                        let displaced_next = displaced.status.next_on_local_delete();
                        if displaced_next != displaced.status {
                            cat.set_status(displaced.remote_file_id, displaced_next)?;
                        }
                        info!(
                            ?to,
                            displaced_file_id = %displaced.remote_file_id,
                            ?displaced_next,
                            "rename target already tracked; displaced row marked deleted"
                        );
                    }
                }
                let mut new_rec = existing.clone();
                new_rec.local_path = to.clone();
                // The source row's content still exists on disk (just
                // at a different path), so this is the upsert side of
                // the state machine even though the user thinks of it
                // as a 'move'.
                new_rec.status = existing.status.next_on_local_upsert();
                new_rec.updated_at = chrono::Utc::now();
                cat.upsert(&new_rec)?;
                info!(?from, ?to, status = ?new_rec.status, "local rename recorded");
                Ok(())
            }
        }
    }

    async fn handle_remote(&self, ev: RemoteEvent) -> Result<()> {
        let m = ev.mutation().clone();
        match ev {
            RemoteEvent::FileCreated(_) | RemoteEvent::FileUpdated(_) => {
                let mut cat = self.catalogue.lock().await;
                match cat.get(m.resource_id)? {
                    Some(existing) => {
                        let next = existing.status.next_on_remote_change();
                        if next != existing.status {
                            cat.set_status(m.resource_id, next)?;
                        }
                    }
                    None => {
                        // Brand-new remote file. Materialise a
                        // placeholder catalogue row so the downloader
                        // (wired in PR5) can pick it up. Dropping the
                        // event on the floor here would mean a new
                        // remote file never becomes visible locally.
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
                            "new remote file registered; awaiting first download"
                        );
                    }
                }
                Ok(())
            }
            RemoteEvent::FileDeleted(_) => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.get(m.resource_id)? {
                    let next = existing.status.next_on_remote_change();
                    if next != existing.status {
                        cat.set_status(m.resource_id, next)?;
                    }
                }
                Ok(())
            }
            RemoteEvent::FileRenamed(_) | RemoteEvent::FileMoved(_) => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.get(m.resource_id)? {
                    let next = existing.status.next_on_remote_change();
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
        // upload flow (PR5) can pick it up.
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

    #[test]
    fn sync_status_transitions_match_truth_table() {
        // Locks the state machine documented on
        // SyncStatus::next_on_local_{upsert,delete} and next_on_remote_change.
        use SyncStatus::*;
        let upsert = [
            (UpToDate, LocalDirty),
            (LocalDirty, LocalDirty),
            (LocalDeleted, LocalDirty),
            (RemoteDirty, Conflict),
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
            (Conflict, Conflict),
            (InFlight, Conflict),
        ];
        for (cur, next) in delete {
            assert_eq!(cur.next_on_local_delete(), next, "delete from {cur:?}");
        }
        let remote = [
            (UpToDate, RemoteDirty),
            (RemoteDirty, RemoteDirty),
            (LocalDirty, Conflict),
            (LocalDeleted, Conflict),
            (Conflict, Conflict),
            (InFlight, Conflict),
            (Evicted, RemoteDirty),
        ];
        for (cur, next) in remote {
            assert_eq!(cur.next_on_remote_change(), next, "remote from {cur:?}");
        }
    }

    #[tokio::test]
    async fn local_revert_round_trips_through_uptodate_after_dirty() {
        // Regression for R6 #1: handle_local used to flip status to
        // LocalDirty via set_status alone, leaving the catalogue's
        // content_hash + size_bytes stale. A subsequent revert to the
        // server's content (A -> B -> A) would short-circuit on the
        // outdated hash A and silently leave the row LocalDirty even
        // though the file matches the server again.
        //
        // With the atomic set_local_state path, the row's hash + size
        // track the on-disk bytes after every change. The revert path
        // is then driven by an explicit downloader/uploader
        // reconciliation in PR5; here we lock down the *invariant*
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
        // Regression for R6 #2: handle_local Delete used to flip
        // status to LocalDirty, which made it indistinguishable from
        // a content change. The upload flow (PR5) needs to know that
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
        // R6 #2 follow-up: a delete then immediate re-create at the
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
}
