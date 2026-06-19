//! Error type shared across the desktop host.
//!
//! `DesktopError` is `Serialize` so it can cross the Tauri command
//! boundary: a `#[tauri::command]` that returns `Result<_,
//! DesktopError>` surfaces the error to the frontend as a JSON value
//! the UI can render. Shell command failures keep their structured
//! [`CommandError`](zk_sync_shell::CommandError) shape under the
//! `command` variant so the React side can pattern-match on the exact
//! reason (already-registered, not-running, …) the same way the web
//! app does in `frontend/src/api/errors.ts`.

use serde::Serialize;
use thiserror::Error;

#[derive(Debug, Error, Serialize)]
#[serde(tag = "kind", content = "detail", rename_all = "snake_case")]
pub enum DesktopError {
    /// A [`zk_sync_shell::Command`] dispatch failed. Carries the
    /// structured shell error verbatim.
    #[error("shell command: {0}")]
    Command(#[from] zk_sync_shell::CommandError),

    /// Authentication / OAuth flow failure.
    #[error("auth: {0}")]
    Auth(String),

    /// A requested operation the underlying SDK `Command` surface does
    /// not expose. No handler constructs this today, but it is a
    /// member of the serialized error contract the frontend mirrors
    /// (`desktop/src/types.ts` `DesktopError.kind`), so it is retained
    /// as a stable wire variant for handlers added later.
    #[allow(dead_code)]
    #[error("unsupported: {0}")]
    Unsupported(String),

    /// I/O failure (loopback listener, filesystem, …). Stored as a
    /// string because `std::io::Error` is not `Serialize`.
    #[error("io: {0}")]
    Io(String),

    /// Backend API / transport failure.
    #[error("api: {0}")]
    Api(String),
}

impl From<std::io::Error> for DesktopError {
    fn from(e: std::io::Error) -> Self {
        DesktopError::Io(e.to_string())
    }
}

impl From<reqwest::Error> for DesktopError {
    fn from(e: reqwest::Error) -> Self {
        DesktopError::Api(e.to_string())
    }
}

impl From<zk_sync_api::ApiError> for DesktopError {
    fn from(e: zk_sync_api::ApiError) -> Self {
        DesktopError::Api(e.to_string())
    }
}

impl From<zk_sync_auth::AuthError> for DesktopError {
    fn from(e: zk_sync_auth::AuthError) -> Self {
        DesktopError::Auth(e.to_string())
    }
}

impl From<zk_sync_shell::ShellError> for DesktopError {
    fn from(e: zk_sync_shell::ShellError) -> Self {
        DesktopError::Api(e.to_string())
    }
}
