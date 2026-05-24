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
                    cat.set_status(existing.remote_file_id, SyncStatus::LocalDirty)?;
                    info!(?path, size_bytes, "local file marked dirty");
                } else {
                    info!(
                        ?path,
                        size_bytes, "new local file discovered; awaiting first upload"
                    );
                }
                Ok(())
            }
            LocalEvent::Delete { path } => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.by_local_path(&path)? {
                    cat.set_status(existing.remote_file_id, SyncStatus::LocalDirty)?;
                    info!(?path, "local delete marked dirty for remote tombstone");
                }
                Ok(())
            }
            LocalEvent::Rename { from, to } => {
                let mut cat = self.catalogue.lock().await;
                if let Some(existing) = cat.by_local_path(&from)? {
                    let mut new_rec = existing.clone();
                    new_rec.local_path = to.clone();
                    new_rec.status = SyncStatus::LocalDirty;
                    new_rec.updated_at = chrono::Utc::now();
                    cat.upsert(&new_rec)?;
                    info!(?from, ?to, "local rename recorded");
                }
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
                    Some(_existing) => {
                        cat.set_status(m.resource_id, SyncStatus::RemoteDirty)?;
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
                if let Some(_existing) = cat.get(m.resource_id)? {
                    cat.set_status(m.resource_id, SyncStatus::RemoteDirty)?;
                }
                Ok(())
            }
            RemoteEvent::FileRenamed(_) | RemoteEvent::FileMoved(_) => {
                let mut cat = self.catalogue.lock().await;
                if let Some(_existing) = cat.get(m.resource_id)? {
                    cat.set_status(m.resource_id, SyncStatus::RemoteDirty)?;
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
        let cat = Catalogue::open(tempdir.path().join("cat.db")).unwrap();
        let catalogue = Arc::new(Mutex::new(cat));
        let client = Arc::new(
            zk_sync_api::Client::builder("https://example.com")
                .build()
                .unwrap(),
        );
        let engine = Engine::new(
            EngineConfig {
                workspace_id: Uuid::new_v4(),
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
}
