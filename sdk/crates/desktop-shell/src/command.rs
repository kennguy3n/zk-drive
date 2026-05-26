//! Command surface — the wire-format requests the GUI frontend
//! sends to the shell.
//!
//! Commands are deliberately small: a single tagged enum lets a
//! Tauri / Electron host serialise the user's intent without ever
//! reaching into the engine's internals. The shell dispatches each
//! command through [`crate::App::dispatch`] and returns a
//! [`CommandResult`] that's equally serde-friendly.
//!
//! ## Wire format
//!
//! `Command` is `#[serde(tag = "type", content = "data")]` —
//! identical shape to [`crate::ShellEvent`]. A Tauri host can run a
//! single `invoke` handler that accepts a JSON blob and dispatches
//! to the shell directly, no per-variant glue code needed.
//!
//! ## Append-only
//!
//! New commands are additive. Renaming a variant or its `tag` value
//! is a wire-format break for older Tauri builds that haven't been
//! upgraded yet, so we treat the set the same way as the error
//! codes in `api/middleware/error_codes.go`: only grow.

use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

use crate::state::WorkspaceState;
use crate::TrayState;

/// Request the GUI frontend ships to the shell. Every variant maps
/// 1:1 to a method on [`crate::App`]; the variant is the wire
/// representation of "user clicked this".
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", content = "data", rename_all = "snake_case")]
pub enum Command {
    /// Register a new workspace. Idempotent if a workspace with the
    /// same `workspace_id` is already known **and** has the same
    /// `root`; otherwise returns
    /// [`CommandError::AlreadyRegistered`] so the user can't
    /// silently re-point a workspace at a different folder and
    /// trip the catalogue's workspace-binding check.
    AddWorkspace {
        workspace_id: Uuid,
        label: String,
        root: std::path::PathBuf,
    },
    /// Drop a workspace from the registry. Stops any running task
    /// first. The on-disk catalogue is left alone — the frontend
    /// should call [`Command::RemoveLocalCache`] to delete it
    /// explicitly. Two-step deletion is a deliberate UX choice: a
    /// misclicked "remove" must be recoverable.
    RemoveWorkspace { workspace_id: Uuid },
    /// Delete a workspace's local catalogue file. Only valid for a
    /// workspace that is **not** currently running — the shell
    /// returns [`CommandError::AlreadyRunning`] otherwise so the
    /// engine doesn't keep writing to a stale connection.
    RemoveLocalCache { workspace_id: Uuid },
    /// Start the background sync loop for a workspace. No-op if
    /// already running.
    StartSync { workspace_id: Uuid },
    /// Stop the background sync loop for a workspace. Drops the
    /// channels into the engine so the poller + engine tasks exit
    /// gracefully on their next select tick. No-op if already
    /// stopped.
    StopSync { workspace_id: Uuid },
    /// Read one workspace's last-known state.
    GetStatus { workspace_id: Uuid },
    /// Read the aggregate tray state.
    GetTrayState,
    /// Enumerate every registered workspace's state. Frontends call
    /// this on launch to render the workspace-list view before the
    /// first event arrives.
    ListWorkspaces,
}

/// Reply shape for [`crate::App::dispatch`]. Each `Command` maps to
/// exactly one variant.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "type", content = "data", rename_all = "snake_case")]
pub enum CommandResult {
    /// Returned by mutation-shaped commands (Add/Remove/Start/Stop
    /// /RemoveLocalCache) that don't have a payload of their own.
    Ok,
    /// Single-workspace status reply for [`Command::GetStatus`].
    Status(WorkspaceState),
    /// Multi-workspace status reply for [`Command::ListWorkspaces`].
    Workspaces(Vec<WorkspaceState>),
    /// Tray aggregate reply for [`Command::GetTrayState`].
    Tray(TrayState),
}

/// Typed reason a command failed. The frontend pattern-matches on
/// this to choose the right user-facing string (one per locale)
/// without ever reading the inner message — see also
/// `frontend/src/api/errors.ts` for the same pattern on the web
/// side.
#[derive(Debug, Clone, Error, Serialize, Deserialize)]
#[serde(tag = "code", content = "message", rename_all = "snake_case")]
pub enum CommandError {
    #[error("workspace {0} is already registered")]
    AlreadyRegistered(Uuid),
    #[error("workspace {0} is not registered")]
    NotRegistered(Uuid),
    #[error("workspace {0} is already running")]
    AlreadyRunning(Uuid),
    #[error("workspace {0} is not running")]
    NotRunning(Uuid),
    /// Workspace was registered with a different `root` than the
    /// new request — surfaces the catalogue's workspace-binding
    /// constraint to the GUI so the user gets a clear error
    /// instead of a stack trace.
    #[error("workspace {workspace_id} is registered with a different root ({existing})")]
    RootMismatch {
        workspace_id: Uuid,
        existing: String,
    },
    /// Generic catch-all for I/O, serde, or engine errors that
    /// don't deserve their own variant. The frontend should show
    /// the message verbatim only if no other variant applies.
    #[error("{0}")]
    Other(String),
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    #[test]
    fn command_round_trips_through_json() {
        let cmd = Command::AddWorkspace {
            workspace_id: Uuid::nil(),
            label: "Acme".into(),
            root: PathBuf::from("/tmp/acme"),
        };
        let json = serde_json::to_string(&cmd).unwrap();
        assert!(json.contains("\"type\":\"add_workspace\""));
        let parsed: Command = serde_json::from_str(&json).unwrap();
        assert_eq!(parsed, cmd);
    }

    #[test]
    fn command_result_uses_snake_case_tag() {
        // Pin the wire format -- Tauri frontends switch on the tag
        // value so renaming `ok` would break already-deployed
        // builds.
        let r = CommandResult::Ok;
        let s = serde_json::to_string(&r).unwrap();
        assert_eq!(s, r#"{"type":"ok"}"#);
    }

    #[test]
    fn command_error_round_trips_through_json() {
        let err = CommandError::RootMismatch {
            workspace_id: Uuid::nil(),
            existing: "/old".into(),
        };
        let json = serde_json::to_string(&err).unwrap();
        assert!(json.contains("\"code\":\"root_mismatch\""));
        let parsed: CommandError = serde_json::from_str(&json).unwrap();
        assert!(matches!(parsed, CommandError::RootMismatch { .. }));
    }
}
