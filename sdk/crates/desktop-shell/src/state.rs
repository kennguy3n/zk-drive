//! Public state types reported to the frontend.

use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// Lifecycle health of one workspace's sync loop. Distinct from the
/// per-file [`zk_sync_engine::SyncStatus`] — this is the "is the
/// engine running and reaching the server" answer the tray icon
/// renders, while `SyncStatus` is the per-file reconciliation state.
///
/// The states are deliberately coarse so a Tauri / Electron host
/// can render a tray badge without re-implementing the engine's
/// internal state machine. The shell derives transitions from
/// catalogue summaries plus task health, so a frontend only ever
/// reads them through [`crate::ShellEvent::HealthChanged`].
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SyncHealth {
    /// Workspace registered, no background tasks running yet. The
    /// frontend should show "paused" in the tray.
    Stopped,
    /// Background tasks just started — catch-up walk in progress,
    /// no live socket yet. Frontend should show a spinner.
    Starting,
    /// Catch-up complete, live socket connected, no pending work.
    /// Tray icon = solid checkmark.
    Idle,
    /// Catch-up complete, live socket connected, files in flight.
    /// Tray icon = animated arrows.
    Syncing,
    /// At least one row in [`zk_sync_engine::SyncStatus::Conflict`].
    /// Tray icon = exclamation, badge with count.
    Conflict,
    /// The poller's exponential backoff is active or the engine
    /// task crashed. Frontend should show a warning + reason.
    Error,
}

impl SyncHealth {
    /// Returns true if the workspace's background task is supposed
    /// to be running right now. Used by [`crate::App::stop`] /
    /// `start` to decide whether a transition is a no-op.
    pub fn is_running(self) -> bool {
        matches!(
            self,
            SyncHealth::Starting | SyncHealth::Idle | SyncHealth::Syncing | SyncHealth::Conflict
        )
    }
}

/// One workspace's lifecycle + last-known summary. Returned from
/// [`crate::Command::GetStatus`] so a frontend can render a "per-
/// workspace row" list view without subscribing to events first.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct WorkspaceState {
    pub workspace_id: Uuid,
    /// Human-readable label the user chose at add-workspace time.
    /// Distinct from `workspace_id` so the tray menu can show
    /// "Acme Corp" rather than a UUID; the engine never reads it.
    pub label: String,
    pub root: std::path::PathBuf,
    pub health: SyncHealth,
    pub summary: crate::Summary,
    /// Last reason the workspace transitioned to
    /// [`SyncHealth::Error`], if any. Reset on the next successful
    /// `Idle` transition so a stale error doesn't pin the tray
    /// after a recovery.
    pub last_error: Option<String>,
    pub last_updated: chrono::DateTime<chrono::Utc>,
}
