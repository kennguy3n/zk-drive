//! Bridges the desktop-shell event bus to the Tauri webview.
//!
//! The shell ([`zk_sync_shell::App`]) is constructed with a
//! [`BroadcastSink`]; every state transition (uploads, downloads,
//! conflicts, health, tray-state) is published on it as a
//! [`ShellEvent`]. The host subscribes once and forwards each event
//! verbatim to the frontend on the `"sync"` channel, exactly as the
//! "Driving the shell from a GUI host" section of `sdk/README.md`
//! describes:
//!
//! ```text
//! // Tauri: window.emit("sync", &ev).unwrap();
//! ```
//!
//! We emit app-wide (`AppHandle::emit`) rather than to a single
//! window so every open window (main + any future detached conflict
//! window) receives the stream. The shell guarantees the latest
//! state is always reachable via the `get_status` / `list_workspaces`
//! commands, so a dropped (lagged) broadcast message never leaves the
//! UI permanently stale.

use std::sync::Arc;

use tauri::{AppHandle, Emitter};
use tokio::sync::broadcast::error::RecvError;
use zk_sync_shell::{BroadcastSink, ShellEvent};

/// Channel name the frontend listens on (`listen("sync", …)`).
pub const SYNC_EVENT: &str = "sync";

/// Subscribe to the shell's [`BroadcastSink`] and forward every
/// [`ShellEvent`] to the Tauri webview. Spawns a detached task on the
/// Tauri async runtime; returns immediately.
pub fn spawn_forwarder(app: AppHandle, sink: Arc<BroadcastSink>) {
    tauri::async_runtime::spawn(async move {
        let mut rx = sink.subscribe();
        loop {
            match rx.recv().await {
                Ok(ev) => {
                    // Keep the native tray in lock-step with the
                    // aggregate state the shell just computed.
                    if let ShellEvent::TrayChanged { tray } = &ev {
                        crate::tray::update_tray(&app, tray);
                    }
                    if let Err(err) = app.emit(SYNC_EVENT, &ev) {
                        tracing::warn!(%err, "failed to emit shell event to webview");
                    }
                }
                // A slow consumer fell behind: the broadcast channel
                // dropped `n` of the oldest messages. The next
                // command-driven refresh re-syncs the UI, so log and
                // keep going rather than tearing down the bridge.
                Err(RecvError::Lagged(n)) => {
                    tracing::warn!(dropped = n, "shell event bridge lagged");
                }
                // Sender gone (app shutting down): exit the loop.
                Err(RecvError::Closed) => {
                    tracing::info!("shell event bridge closed");
                    break;
                }
            }
        }
    });
}
