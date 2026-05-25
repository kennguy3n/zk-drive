//! Bidirectional file-system ↔ ZK Drive sync engine.
//!
//! The engine combines four moving parts:
//!
//!   * a [`Catalogue`] (SQLite) tracking, per file path: remote ID,
//!     last-known version, content hash, last-applied change-feed
//!     sequence, and a `pinned` flag for the offline cache path.
//!   * a [`Watcher`] that wraps [`notify`] and emits coalesced
//!     [`LocalEvent`]s.
//!   * a [`RemotePoller`] that consumes the workspace change feed
//!     ([`zk-sync-api::ChangefeedClient`]) and emits [`RemoteEvent`]s
//!     to the engine.
//!   * an [`Engine::run`] loop that reconciles local and remote
//!     events into upload / download / conflict actions.
//!
//! All blocking I/O is dispatched to `tokio::task::spawn_blocking` so
//! the engine is safe to drive from a single-threaded runtime (e.g.
//! the Tauri main thread).

mod catalogue;
mod conflict;
mod connectivity;
mod engine;
mod events;
mod eviction;
mod hash;
mod poller;
mod status;
mod watcher;

pub use catalogue::{Catalogue, FileRecord, SyncStatus};
pub use conflict::ConflictPolicy;
pub use connectivity::{ConnectivityState, OnlineState};
pub use engine::{
    placeholder_dir, tombstone_dir, Engine, EngineConfig, PLACEHOLDER_DIR_NAME, TOMBSTONE_DIR_NAME,
};
pub use events::{LocalEvent, RemoteEvent};
pub use eviction::{evict_to_quota, EvictionReport, EvictionTrigger};
pub use hash::content_hash;
pub use poller::RemotePoller;
pub use status::{ConnectivityStateOwned, Snapshot as StatusSnapshot, SyncStatusCounts};
pub use watcher::Watcher;

use thiserror::Error;

#[derive(Debug, Error)]
pub enum SyncError {
    #[error("sync: io: {0}")]
    Io(#[from] std::io::Error),
    #[error("sync: sqlite: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("sync: notify: {0}")]
    Notify(#[from] notify::Error),
    #[error("sync: api: {0}")]
    Api(#[from] zk_sync_api::ApiError),
    #[error("sync: crypto: {0}")]
    Crypto(#[from] zk_sync_crypto::Error),
    #[error("sync: auth: {0}")]
    Auth(#[from] zk_sync_auth::AuthError),
    #[error("sync: {0}")]
    Other(String),
}

pub type Result<T> = std::result::Result<T, SyncError>;
