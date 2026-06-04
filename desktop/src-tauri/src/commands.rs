//! Tauri command handlers.
//!
//! Each handler is a thin adapter: it deserialises the frontend's
//! request, ferries it to the shell as a
//! [`Command`](zk_sync_shell::Command) via
//! [`App::dispatch`](zk_sync_shell::App::dispatch), and returns the
//! typed [`CommandResult`](zk_sync_shell::CommandResult) payload. The
//! shell owns all engine wiring; this layer never reaches into the
//! engine directly.
//!
//! Repo deviation (see `desktop/README.md`): the task brief lists
//! `PauseSync` / `ResumeSync` / `ResolveConflict` / `SetFolderPolicy`
//! commands. The desktop-shell `Command` enum at this SDK revision
//! exposes `StopSync` / `StartSync` (which are exactly pause/resume
//! of the per-workspace sync loop) but not conflict-resolution or
//! per-folder selective-sync variants. We therefore map
//! pause→`StopSync` and resume→`StartSync`, and expose
//! `resolve_conflict` / `set_folder_policy` commands that return a
//! structured `Unsupported` error until the SDK grows the matching
//! `Command` variants — keeping the frontend contract forward-stable
//! without modifying the SDK crates (owned by other sessions).

use tauri::State;
use uuid::Uuid;
use zk_sync_shell::{Command, CommandResult, TrayState, WorkspaceState};

use crate::auth::{self, Provider};
use crate::error::DesktopError;
use crate::DesktopState;

/// Register a new workspace binding (folder ↔ workspace) and return
/// its freshly-seeded [`WorkspaceState`].
#[tauri::command]
pub async fn add_workspace(
    state: State<'_, DesktopState>,
    label: String,
    root: String,
) -> Result<WorkspaceState, DesktopError> {
    let workspace_id = Uuid::new_v4();
    state
        .shell
        .dispatch(Command::AddWorkspace {
            workspace_id,
            label,
            root: root.into(),
        })
        .await?;
    get_status(state, workspace_id).await
}

/// Drop a workspace from the registry (stops its sync loop first).
/// The local catalogue is preserved; call [`remove_local_cache`] to
/// delete it.
#[tauri::command]
pub async fn remove_workspace(
    state: State<'_, DesktopState>,
    workspace_id: Uuid,
) -> Result<(), DesktopError> {
    state
        .shell
        .dispatch(Command::RemoveWorkspace { workspace_id })
        .await?;
    Ok(())
}

/// Delete a stopped workspace's local SQLite catalogue.
#[tauri::command]
pub async fn remove_local_cache(
    state: State<'_, DesktopState>,
    workspace_id: Uuid,
) -> Result<(), DesktopError> {
    state
        .shell
        .dispatch(Command::RemoveLocalCache { workspace_id })
        .await?;
    Ok(())
}

/// Pause a workspace's background sync loop (maps to the shell's
/// `StopSync`).
#[tauri::command]
pub async fn pause_sync(
    state: State<'_, DesktopState>,
    workspace_id: Uuid,
) -> Result<(), DesktopError> {
    state
        .shell
        .dispatch(Command::StopSync { workspace_id })
        .await?;
    Ok(())
}

/// Resume a workspace's background sync loop (maps to the shell's
/// `StartSync`). Requires an API client — i.e. the user must be
/// logged in.
#[tauri::command]
pub async fn resume_sync(
    state: State<'_, DesktopState>,
    workspace_id: Uuid,
) -> Result<(), DesktopError> {
    state
        .shell
        .dispatch(Command::StartSync { workspace_id })
        .await?;
    Ok(())
}

/// Enumerate every registered workspace's last-known state.
#[tauri::command]
pub async fn list_workspaces(
    state: State<'_, DesktopState>,
) -> Result<Vec<WorkspaceState>, DesktopError> {
    match state.shell.dispatch(Command::ListWorkspaces).await? {
        CommandResult::Workspaces(ws) => Ok(ws),
        other => Err(unexpected(other)),
    }
}

/// One workspace's current state.
#[tauri::command]
pub async fn get_status(
    state: State<'_, DesktopState>,
    workspace_id: Uuid,
) -> Result<WorkspaceState, DesktopError> {
    match state
        .shell
        .dispatch(Command::GetStatus { workspace_id })
        .await?
    {
        CommandResult::Status(s) => Ok(s),
        other => Err(unexpected(other)),
    }
}

/// The cross-workspace tray aggregate (same value the native tray
/// renders) so the dashboard header can show a single status pill.
#[tauri::command]
pub async fn get_tray_state(state: State<'_, DesktopState>) -> Result<TrayState, DesktopError> {
    match state.shell.dispatch(Command::GetTrayState).await? {
        CommandResult::Tray(t) => Ok(t),
        other => Err(unexpected(other)),
    }
}

// ---- Authentication -------------------------------------------------

/// Whether a persisted bearer exists for the configured backend.
#[tauri::command]
pub async fn auth_status(state: State<'_, DesktopState>) -> Result<bool, DesktopError> {
    Ok(auth::is_logged_in(&state.base_url).await)
}

/// Run the interactive OAuth2 PKCE login for `provider` and, on
/// success, attach a refreshing API client to the shell so sync can
/// start. Returns `true` once a token is persisted.
#[tauri::command]
pub async fn begin_login(
    state: State<'_, DesktopState>,
    provider: String,
) -> Result<bool, DesktopError> {
    let provider = Provider::parse(&provider)?;
    auth::login(&state.base_url, provider).await?;
    // Attach a refreshing client so StartSync has a bearer. `set_client`
    // is idempotent (OnceLock); the client reads the keychain on every
    // request, so re-login after token rotation is picked up without a
    // fresh client.
    let client = auth::build_client(&state.base_url)?;
    state.shell.set_client(client);
    Ok(true)
}

/// Forget the persisted bearer (logout).
#[tauri::command]
pub async fn logout(state: State<'_, DesktopState>) -> Result<(), DesktopError> {
    auth::logout(&state.base_url).await
}

// ---- Not-yet-supported by the SDK Command surface ------------------

/// Selective-sync per-folder policy. The desktop-shell `Command`
/// surface does not yet expose a `SetFolderPolicy` variant, so this
/// returns a structured `Unsupported` error the UI renders as a
/// "coming soon" state rather than silently no-op'ing.
#[tauri::command]
pub async fn set_folder_policy(
    _state: State<'_, DesktopState>,
    _workspace_id: Uuid,
    _folder_id: String,
    _policy: String,
) -> Result<(), DesktopError> {
    Err(DesktopError::Unsupported(
        "selective sync (SetFolderPolicy) is not exposed by the current SDK Command surface".into(),
    ))
}

/// Conflict resolution. The shell surfaces conflicts via
/// `HealthChanged{Conflict}` + the per-workspace summary, but does not
/// yet expose a `ResolveConflict` command, so this returns a
/// structured `Unsupported` error.
#[tauri::command]
pub async fn resolve_conflict(
    _state: State<'_, DesktopState>,
    _workspace_id: Uuid,
    _file_id: String,
    _resolution: String,
) -> Result<(), DesktopError> {
    Err(DesktopError::Unsupported(
        "conflict resolution (ResolveConflict) is not exposed by the current SDK Command surface"
            .into(),
    ))
}

fn unexpected(result: CommandResult) -> DesktopError {
    DesktopError::Api(format!("unexpected shell reply: {result:?}"))
}
