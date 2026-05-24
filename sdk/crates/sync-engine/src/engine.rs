//! Engine reconciliation loop: consumes [`LocalEvent`] + [`RemoteEvent`]
//! channels, drives the catalogue, and dispatches upload / download
//! tasks against [`zk_sync_api`].

use std::path::PathBuf;
use std::sync::Arc;

use tokio::sync::{mpsc, Mutex};
use tracing::{info, warn};

use uuid::Uuid;
use zk_sync_api::Client;

use crate::catalogue::{Catalogue, SyncStatus};
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
                if let Some(_existing) = cat.get(m.resource_id)? {
                    cat.set_status(m.resource_id, SyncStatus::RemoteDirty)?;
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
