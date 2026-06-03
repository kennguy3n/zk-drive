//! System-tray icon driven by the shell's cross-workspace
//! [`TrayState`] aggregate.
//!
//! The shell reduces every workspace's [`SyncHealth`] into a single
//! [`TrayState`] (priority: Error → Conflict → Syncing → Idle →
//! Starting → Stopped) and broadcasts a
//! [`ShellEvent::TrayChanged`](zk_sync_shell::ShellEvent::TrayChanged)
//! whenever it changes. [`events::spawn_forwarder`](crate::events)
//! calls [`update_tray`] on each such event so the native tray
//! tooltip / title stay in lock-step with the UI.

use tauri::menu::{Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, Runtime};
use zk_sync_shell::{SyncHealth, TrayState};

/// Stable id so [`update_tray`] can look the icon up via
/// [`AppHandle::tray_by_id`].
pub const TRAY_ID: &str = "zk-drive-tray";

/// Build the tray icon, its context menu, and the click handlers.
/// Called once from the Tauri `setup` hook.
pub fn build_tray<R: Runtime>(app: &AppHandle<R>) -> tauri::Result<()> {
    let open_item = MenuItem::with_id(app, "open", "Open ZK Drive", true, None::<&str>)?;
    let separator = PredefinedMenuItem::separator(app)?;
    let quit_item = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
    let menu = Menu::with_items(app, &[&open_item, &separator, &quit_item])?;

    let mut builder = TrayIconBuilder::with_id(TRAY_ID)
        .menu(&menu)
        .show_menu_on_left_click(false)
        .tooltip("ZK Drive — starting…")
        .on_menu_event(|app, event| match event.id.as_ref() {
            "open" => show_main_window(app),
            "quit" => app.exit(0),
            _ => {}
        })
        .on_tray_icon_event(|tray, event| {
            // Left-click the icon to reveal the main window — the
            // standard "click the tray to open" desktop affordance.
            if let TrayIconEvent::Click {
                button: MouseButton::Left,
                button_state: MouseButtonState::Up,
                ..
            } = event
            {
                show_main_window(tray.app_handle());
            }
        });

    // Reuse the bundled application icon for the tray. If the build
    // has no window icon configured we still create the tray (it
    // renders the platform default) rather than failing startup.
    if let Some(icon) = app.default_window_icon() {
        builder = builder.icon(icon.clone());
    }

    builder.build(app)?;
    Ok(())
}

/// Refresh the tray tooltip + title from a fresh [`TrayState`].
/// No-op if the tray was never built (e.g. headless test host).
pub fn update_tray<R: Runtime>(app: &AppHandle<R>, state: &TrayState) {
    let Some(tray) = app.tray_by_id(TRAY_ID) else {
        return;
    };
    let _ = tray.set_tooltip(Some(tooltip_for(state)));
    // `set_title` renders next to the icon on macOS and is a no-op
    // on Windows / Linux, so it is safe to call unconditionally.
    let _ = tray.set_title(Some(title_for(state)));
}

/// Short status word shown next to the macOS menu-bar icon.
fn title_for(state: &TrayState) -> String {
    match state.health {
        SyncHealth::Error => "!".to_string(),
        SyncHealth::Conflict => format!("⚠ {}", state.total_conflicts),
        SyncHealth::Syncing => format!("↻ {}", state.total_pending),
        SyncHealth::Idle => "✓".to_string(),
        SyncHealth::Starting => "…".to_string(),
        SyncHealth::Stopped => String::new(),
    }
}

/// Multi-detail tooltip text. Single-line per-platform conventions,
/// but we pack the high-signal counts a user wants at a glance.
fn tooltip_for(state: &TrayState) -> String {
    let head = match state.health {
        SyncHealth::Error => "Sync error",
        SyncHealth::Conflict => "Conflicts need attention",
        SyncHealth::Syncing => "Syncing…",
        SyncHealth::Idle => "Up to date",
        SyncHealth::Starting => "Starting…",
        SyncHealth::Stopped => "Paused",
    };
    if let (SyncHealth::Error, Some(err)) = (state.health, state.first_error.as_ref()) {
        return format!("ZK Drive — {head}: {err}");
    }
    format!(
        "ZK Drive — {head} ({}/{} workspaces, {} pending, {} conflicts)",
        state.workspaces_running, state.workspaces, state.total_pending, state.total_conflicts
    )
}

/// Show + focus the main window, creating nothing — the window is
/// declared in `tauri.conf.json` and merely hidden on close.
fn show_main_window<R: Runtime>(app: &AppHandle<R>) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}
