//! `zk-sync-shell` — embedding-friendly desktop sync shell.
//!
//! The crate wraps [`zk_sync_engine::Engine`] in a multi-workspace
//! lifecycle harness whose surface area is two thin, host-agnostic
//! contracts:
//!
//!   * a serde-shaped [`Command`] enum that any GUI / IPC frontend
//!     (Tauri, Electron, JSON-RPC over UDS, native UniFFI, …) can
//!     ferry from user actions to the engine, and
//!   * a serde-shaped [`ShellEvent`] enum that the shell pushes to
//!     subscribers (via the [`EventSink`] trait) whenever sync state
//!     changes — file uploads, downloads, conflicts, health, tray-
//!     state transitions.
//!
//! The host (Tauri main, Electron preload, etc.) owns the GUI
//! framework choice; this crate stays pure-Rust and headless so the
//! `cargo test --workspace` CI job doesn't pull in webkit2gtk /
//! Cocoa / WebView2.
//!
//! ## Why a separate crate
//!
//! The [`Engine`](zk_sync_engine::Engine) is single-workspace by
//! design — its catalogue is bound to one workspace at open time so
//! the SQLite `files` table can't co-mingle rows from two
//! workspaces and corrupt the conflict state machine. A real desktop
//! product needs to sync multiple workspaces simultaneously, so the
//! shell layer owns the *registry* of bindings, their lifecycles,
//! and the cross-workspace aggregations (tray state, total file
//! count, conflict total) that the GUI surfaces.
//!
//! ## What lives where
//!
//! ```text
//! +-------------------------------------------------------------+
//! |  GUI frontend (Tauri main, Electron, …)                     |
//! |   ↑   ShellEvent (broadcast)        ↓  Command (dispatch)   |
//! +-------------------------------------------------------------+
//! |  zk-sync-shell::App                                         |
//! |   - WorkspaceBinding { catalogue, watcher, poller, engine } |
//! |   - lifecycle (start / stop / health-poll)                  |
//! |   - tray-state aggregator                                   |
//! |   - persistent AppConfig (JSON sidecar)                     |
//! +-------------------------------------------------------------+
//! |  zk-sync-engine, zk-sync-api, zk-sync-auth, zk-sync-crypto   |
//! +-------------------------------------------------------------+
//! ```

mod app;
mod command;
mod config;
mod event;
mod state;
mod summary;
mod tray;

pub use app::{App, AppHandle, HEALTH_POLL_INTERVAL};
pub use command::{Command, CommandError, CommandResult, ConflictResolution, FolderPolicy};
pub use config::{AppConfig, WorkspaceEntry};
pub use event::{BroadcastSink, EventSink, ShellEvent, TaskKind};
pub use state::{SyncHealth, WorkspaceState};
pub use summary::Summary;
pub use tray::TrayState;

use thiserror::Error;

#[derive(Debug, Error)]
pub enum ShellError {
    #[error("shell: io: {0}")]
    Io(#[from] std::io::Error),
    #[error("shell: sync engine: {0}")]
    Engine(#[from] zk_sync_engine::SyncError),
    #[error("shell: api: {0}")]
    Api(#[from] zk_sync_api::ApiError),
    #[error("shell: serde: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("shell: workspace {0} is already registered")]
    AlreadyRegistered(uuid::Uuid),
    #[error("shell: workspace {0} is not registered")]
    NotRegistered(uuid::Uuid),
    #[error("shell: workspace {0} is already running")]
    AlreadyRunning(uuid::Uuid),
    #[error("shell: workspace {0} is not running")]
    NotRunning(uuid::Uuid),
    #[error("shell: {0}")]
    Other(String),
}

pub type Result<T> = std::result::Result<T, ShellError>;
