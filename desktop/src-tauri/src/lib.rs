//! ZK Drive desktop sync client — Tauri v2 host.
//!
//! Wires the pure-Rust [`zk_sync_shell::App`] (the multi-workspace
//! sync harness) into a Tauri window + system tray, exactly along the
//! lines of the "Driving the shell from a GUI host" section of
//! `sdk/README.md`:
//!
//!   * [`App::with_config_path`] is constructed with a
//!     [`BroadcastSink`]; [`App::resume_persisted`] re-registers the
//!     workspaces from the JSON sidecar and [`App::spawn_health_loop`]
//!     starts the 1 Hz catalogue sampler.
//!   * [`events::spawn_forwarder`] subscribes to the sink and emits
//!     every [`ShellEvent`](zk_sync_shell::ShellEvent) to the webview
//!     on the `"sync"` channel.
//!   * [`commands`] exposes the [`Command`](zk_sync_shell::Command)
//!     surface to the frontend as Tauri commands.
//!   * [`tray`] renders the cross-workspace
//!     [`TrayState`](zk_sync_shell::TrayState).
//!
//! The crate is a library (+ a thin `main.rs` shim) so a future
//! `tauri android`/`ios` entrypoint can reuse [`run`].

mod auth;
mod commands;
mod error;
mod events;
mod tray;

use std::path::PathBuf;
use std::sync::Arc;

use tauri::WindowEvent;
use zk_sync_shell::{App, AppHandle, BroadcastSink, EventSink};

/// Default backend if `ZK_DRIVE_BASE_URL` is unset. Overridable so a
/// dev / staging build can point at a different gateway without a
/// rebuild.
const DEFAULT_BASE_URL: &str = "https://drive.example.com";

/// Application state shared with every Tauri command handler.
pub struct DesktopState {
    /// The multi-workspace sync shell. `Arc<App>` so each command
    /// handler (its own tokio task) can dispatch concurrently.
    pub shell: AppHandle,
    /// Backend base URL the OAuth flow + API client target.
    pub base_url: String,
}

/// Resolve the JSON sidecar path the shell persists its workspace
/// registry to (`<config-dir>/zk-drive/app.json`).
fn config_path() -> PathBuf {
    dirs::config_dir()
        .unwrap_or_else(std::env::temp_dir)
        .join("zk-drive")
        .join("app.json")
}

/// Build and run the desktop application. Blocks until the window is
/// closed / the app exits.
pub fn run() {
    // `try_init` so a host that already installed a subscriber (tests)
    // doesn't panic on a double-init.
    let _ = tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .try_init();

    let base_url =
        std::env::var("ZK_DRIVE_BASE_URL").unwrap_or_else(|_| DEFAULT_BASE_URL.to_string());

    let sink = Arc::new(BroadcastSink::new());
    let shell = App::with_config_path(sink.clone() as Arc<dyn EventSink>, config_path());

    // Clones moved into the setup closure / managed state.
    let shell_for_state = shell.clone();
    let shell_for_setup = shell.clone();
    let base_for_state = base_url.clone();
    let base_for_setup = base_url.clone();
    let sink_for_setup = sink.clone();

    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_fs::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_notification::init())
        .plugin(tauri_plugin_updater::Builder::new().build())
        .manage(DesktopState {
            shell: shell_for_state,
            base_url: base_for_state,
        })
        .setup(move |app| {
            let handle = app.handle().clone();

            // All shell async wiring runs inside a tokio context (the
            // shell uses `tokio::spawn` internally, e.g. in
            // `spawn_health_loop`).
            let shell_async = shell_for_setup.clone();
            let base_async = base_for_setup.clone();
            tauri::async_runtime::spawn(async move {
                if let Err(err) = shell_async.resume_persisted().await {
                    tracing::warn!(%err, "failed to resume persisted workspaces");
                }
                // If a bearer is already in the keychain, attach a
                // refreshing client up front so autostart workspaces
                // can sync without an explicit login.
                if auth::is_logged_in(&base_async).await {
                    match auth::build_client(&base_async) {
                        Ok(client) => shell_async.set_client(client),
                        Err(err) => {
                            tracing::warn!(%err, "failed to build API client from stored token")
                        }
                    }
                }
                // Detached: the loop runs for the app's lifetime. The
                // `App` stays alive via the managed `DesktopState`.
                let _health = shell_async.spawn_health_loop();
            });

            // Forward shell events → webview, and keep the tray in sync.
            events::spawn_forwarder(handle.clone(), sink_for_setup.clone());
            tray::build_tray(&handle)?;
            Ok(())
        })
        // Closing the main window hides it to the tray instead of
        // quitting — the standard sync-client affordance. Quit via the
        // tray menu's "Quit" item.
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                if window.label() == "main" {
                    api.prevent_close();
                    let _ = window.hide();
                }
            }
        })
        .invoke_handler(tauri::generate_handler![
            commands::add_workspace,
            commands::remove_workspace,
            commands::remove_local_cache,
            commands::pause_sync,
            commands::resume_sync,
            commands::list_workspaces,
            commands::get_status,
            commands::get_tray_state,
            commands::auth_status,
            commands::begin_login,
            commands::logout,
            commands::set_folder_policy,
            commands::resolve_conflict,
        ])
        .run(tauri::generate_context!())
        .expect("error while running ZK Drive desktop application");
}
