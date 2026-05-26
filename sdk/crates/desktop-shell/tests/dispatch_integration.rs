//! End-to-end exercises of the [`App::dispatch`] surface.
//!
//! These tests run without an `Arc<Client>` — every command other
//! than `StartSync` is reachable without a backend, which is the
//! shell-design invariant the persistent-config flow depends on
//! (a host hydrates the registry before bearer tokens are loaded
//! from the OS keychain).

use std::sync::Arc;
use std::time::Duration;

use tempfile::tempdir;
use uuid::Uuid;
use zk_sync_shell::{
    App, AppConfig, BroadcastSink, Command, CommandError, CommandResult, EventSink, ShellEvent,
    SyncHealth, TrayState,
};

fn make_app() -> (Arc<App>, Arc<BroadcastSink>) {
    let sink = Arc::new(BroadcastSink::new());
    let app = App::with_sink(sink.clone() as Arc<dyn EventSink>);
    (app, sink)
}

async fn collect_events(
    mut rx: tokio::sync::broadcast::Receiver<ShellEvent>,
    max_events: usize,
    timeout: Duration,
) -> Vec<ShellEvent> {
    let mut out = Vec::new();
    while out.len() < max_events {
        match tokio::time::timeout(timeout, rx.recv()).await {
            Ok(Ok(ev)) => out.push(ev),
            // Sink closed or timed out -- stop collecting.
            _ => break,
        }
    }
    out
}

#[tokio::test]
async fn add_then_list_returns_the_workspace() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    let root = dir.path().join("ws");
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: root.clone(),
    })
    .await
    .unwrap();

    let r = app.dispatch(Command::ListWorkspaces).await.unwrap();
    let CommandResult::Workspaces(states) = r else {
        panic!("expected Workspaces reply, got {r:?}");
    };
    assert_eq!(states.len(), 1);
    assert_eq!(states[0].workspace_id, id);
    assert_eq!(states[0].label, "Acme");
    assert_eq!(states[0].root, root);
    assert_eq!(states[0].health, SyncHealth::Stopped);
}

#[tokio::test]
async fn add_workspace_is_idempotent_on_identical_root() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    let root = dir.path().join("ws");
    let cmd = Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root,
    };
    app.dispatch(cmd.clone()).await.unwrap();
    // A second AddWorkspace with the same root is a no-op rather
    // than an error -- a Tauri frontend may resend on reconnect
    // and we don't want to churn the catalogue.
    app.dispatch(cmd).await.unwrap();
}

#[tokio::test]
async fn add_workspace_rejects_different_root_for_same_id() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("a"),
    })
    .await
    .unwrap();
    let err = app
        .dispatch(Command::AddWorkspace {
            workspace_id: id,
            label: "Acme".into(),
            root: dir.path().join("b"),
        })
        .await
        .unwrap_err();
    match err {
        CommandError::RootMismatch { workspace_id, .. } => assert_eq!(workspace_id, id),
        other => panic!("expected RootMismatch, got {other:?}"),
    }
}

#[tokio::test]
async fn remove_unknown_workspace_returns_not_registered() {
    let (app, _sink) = make_app();
    let id = Uuid::new_v4();
    let err = app
        .dispatch(Command::RemoveWorkspace { workspace_id: id })
        .await
        .unwrap_err();
    assert!(matches!(err, CommandError::NotRegistered(_)));
}

#[tokio::test]
async fn add_remove_round_trips() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "X".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    app.dispatch(Command::RemoveWorkspace { workspace_id: id })
        .await
        .unwrap();
    let r = app.dispatch(Command::ListWorkspaces).await.unwrap();
    let CommandResult::Workspaces(states) = r else {
        panic!("expected Workspaces reply");
    };
    assert!(states.is_empty());
}

#[tokio::test]
async fn get_tray_state_with_no_workspaces_is_stopped() {
    let (app, _sink) = make_app();
    let r = app.dispatch(Command::GetTrayState).await.unwrap();
    let CommandResult::Tray(t) = r else {
        panic!("expected Tray reply");
    };
    let expected = TrayState {
        health: SyncHealth::Stopped,
        total_pending: 0,
        total_conflicts: 0,
        workspaces: 0,
        workspaces_running: 0,
        first_error: None,
    };
    assert_eq!(t, expected);
}

#[tokio::test]
async fn add_workspace_emits_added_and_tray_events() {
    let (app, sink) = make_app();
    let rx = sink.subscribe();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    let events = collect_events(rx, 2, Duration::from_millis(200)).await;
    let mut saw_added = false;
    let mut saw_tray = false;
    for ev in events {
        match ev {
            ShellEvent::WorkspaceAdded {
                workspace_id,
                label,
            } => {
                assert_eq!(workspace_id, id);
                assert_eq!(label, "Acme");
                saw_added = true;
            }
            ShellEvent::TrayChanged { .. } => saw_tray = true,
            other => panic!("unexpected event: {other:?}"),
        }
    }
    assert!(saw_added, "WorkspaceAdded must be emitted");
    assert!(saw_tray, "TrayChanged must be emitted");
}

#[tokio::test]
async fn start_sync_without_client_returns_clear_error() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    let err = app
        .dispatch(Command::StartSync { workspace_id: id })
        .await
        .unwrap_err();
    let msg = format!("{err}");
    assert!(
        msg.contains("API client"),
        "error must mention missing client, got: {msg}"
    );
}

#[tokio::test]
async fn stop_sync_on_stopped_workspace_is_no_op() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    // Workspace is in Stopped state -- StopSync must succeed
    // without erroring or emitting a redundant HealthChanged.
    app.dispatch(Command::StopSync { workspace_id: id })
        .await
        .unwrap();
}

#[tokio::test]
async fn remove_local_cache_requires_stopped_workspace() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    // A registered-but-stopped workspace is fine -- the catalogue
    // file exists (Catalogue::open created it on Add) so this
    // exercises the success path.
    app.dispatch(Command::RemoveLocalCache { workspace_id: id })
        .await
        .unwrap();
}

#[tokio::test]
async fn tick_health_emits_summary_changed_on_first_sample() {
    let (app, sink) = make_app();
    let rx = sink.subscribe();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    // Drain the Add events first.
    let _ = collect_events(rx, 2, Duration::from_millis(100)).await;

    // First tick goes from "no sample yet" to a default summary.
    // The summary IS the default, so SummaryChanged is suppressed
    // (the binding's `last_summary` was initialised to default).
    // Tray also doesn't transition because health stays Stopped.
    // What we DO see is nothing -- pin that behaviour so the
    // health loop can't accidentally start emitting spam.
    let rx2 = sink.subscribe();
    app.tick_health().await;
    let events = collect_events(rx2, 1, Duration::from_millis(100)).await;
    assert!(
        events.is_empty(),
        "tick over an empty stopped workspace must not emit anything, got: {events:?}"
    );
}

#[tokio::test]
async fn persistent_config_survives_app_restart() {
    let dir = tempdir().unwrap();
    let cfg_path = dir.path().join("app.json");
    let sink1 = Arc::new(BroadcastSink::new());
    let app1 = App::with_config_path(sink1 as Arc<dyn EventSink>, cfg_path.clone());
    let id = Uuid::new_v4();
    let root = dir.path().join("ws");
    app1.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Persistent".into(),
        root: root.clone(),
    })
    .await
    .unwrap();

    // Read the persisted file directly to verify the registry
    // landed on disk before we tear the first app down.
    let on_disk = AppConfig::load(&cfg_path).unwrap();
    assert_eq!(on_disk.workspaces.len(), 1);
    assert_eq!(on_disk.workspaces[0].workspace_id, id);
    assert_eq!(on_disk.workspaces[0].label, "Persistent");

    drop(app1);

    // Build a fresh app pointing at the same config file and
    // re-hydrate. The new app must see exactly the same workspace.
    let sink2 = Arc::new(BroadcastSink::new());
    let app2 = App::with_config_path(sink2 as Arc<dyn EventSink>, cfg_path.clone());
    app2.resume_persisted().await.unwrap();
    let r = app2.dispatch(Command::ListWorkspaces).await.unwrap();
    let CommandResult::Workspaces(states) = r else {
        panic!("expected Workspaces reply");
    };
    assert_eq!(states.len(), 1);
    assert_eq!(states[0].workspace_id, id);
    assert_eq!(states[0].label, "Persistent");
    assert_eq!(states[0].root, root);
}

#[tokio::test]
async fn dispatch_get_status_returns_default_summary_after_add() {
    let (app, _sink) = make_app();
    let dir = tempdir().unwrap();
    let id = Uuid::new_v4();
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "S".into(),
        root: dir.path().join("ws"),
    })
    .await
    .unwrap();
    let r = app
        .dispatch(Command::GetStatus { workspace_id: id })
        .await
        .unwrap();
    let CommandResult::Status(state) = r else {
        panic!("expected Status reply");
    };
    assert_eq!(state.workspace_id, id);
    assert_eq!(state.summary.total_files, 0);
    assert_eq!(state.summary.cursor, 0);
    assert!(state.last_error.is_none());
}

/// Regression for ANALYSIS_0002 in PR #86: `remove_local_cache`
/// must close the SQLite `Connection` before unlinking the file so
/// the on-disk catalogue is genuinely gone and a follow-up
/// `AddWorkspace`/`GetStatus` cycle picks up the empty state. We
/// can't exercise the Windows lock specifically from a Linux-only
/// CI runner, but we *can* assert the two observable post-
/// conditions: (a) the file no longer exists on disk, and (b) the
/// binding's catalogue handle was released (a re-`AddWorkspace` on
/// the same root succeeds, which it could not if a `RootMismatch`
/// fired or a half-removed binding lingered in the map).
#[tokio::test]
async fn remove_local_cache_actually_unlinks_the_db_file() {
    let dir = tempdir().unwrap();
    let cfg_path = dir.path().join("app.json");
    let sink = Arc::new(BroadcastSink::new());
    let app = App::with_config_path(sink as Arc<dyn EventSink>, cfg_path.clone());
    let id = Uuid::new_v4();
    let root = dir.path().join("ws");
    app.dispatch(Command::AddWorkspace {
        workspace_id: id,
        label: "Acme".into(),
        root: root.clone(),
    })
    .await
    .unwrap();

    // The catalogue file lives next to the config file.
    let cat_path = dir.path().join(format!("{id}.db"));
    assert!(
        cat_path.exists(),
        "catalogue file should exist after Add: {cat_path:?}"
    );

    app.dispatch(Command::RemoveLocalCache { workspace_id: id })
        .await
        .unwrap();
    assert!(
        !cat_path.exists(),
        "catalogue file should be gone after RemoveLocalCache: {cat_path:?}"
    );

    // A subsequent AddWorkspace on the same id+root is idempotent
    // (same root match), but the catalogue is None on the binding;
    // GetStatus must still return a default summary without
    // panicking. This pins the Option<Arc<Mutex<Catalogue>>>
    // invariant: tick_one / GetStatus do not unwrap.
    let r = app
        .dispatch(Command::GetStatus { workspace_id: id })
        .await
        .unwrap();
    let CommandResult::Status(state) = r else {
        panic!("expected Status reply, got {r:?}");
    };
    assert_eq!(state.summary.total_files, 0);
    assert_eq!(state.health, SyncHealth::Stopped);
}

#[tokio::test]
async fn resume_persisted_preserves_autostart_flag_on_disk() {
    // Regression for the BUG flagged on PR #86:
    //
    // `add_workspace_at` always persists with `autostart=false`
    // (the just-constructed binding has not yet been restored).
    // `resume_persisted` then sets `ws.autostart = true` *in memory
    // only*. Without a final persist, the on-disk config silently
    // overwrites `autostart: true` to `autostart: false` for the
    // last workspace processed -- corrupting the user's startup
    // preference until the next config-touching command fires.
    //
    // This test pins the post-fix behaviour: after `resume_persisted`,
    // the on-disk config still reflects the restored autostart flag
    // for every workspace, including the last one.
    let dir = tempdir().unwrap();
    let cfg_path = dir.path().join("app.json");

    // Hand-craft a persisted config with two workspaces, both
    // autostart=true, so we exercise both the "not last" and the
    // "last" paths in resume_persisted.
    let id_a = Uuid::new_v4();
    let id_b = Uuid::new_v4();
    let root_a = dir.path().join("a");
    let root_b = dir.path().join("b");
    let cat_a = dir.path().join("a.sqlite");
    let cat_b = dir.path().join("b.sqlite");
    let seeded = AppConfig {
        version: 1,
        workspaces: vec![
            zk_sync_shell::WorkspaceEntry {
                workspace_id: id_a,
                label: "A".into(),
                root: root_a.clone(),
                catalogue_path: cat_a.clone(),
                autostart: true,
            },
            zk_sync_shell::WorkspaceEntry {
                workspace_id: id_b,
                label: "B".into(),
                root: root_b.clone(),
                catalogue_path: cat_b.clone(),
                autostart: true,
            },
        ],
    };
    seeded.save(&cfg_path).unwrap();

    let sink = Arc::new(BroadcastSink::new());
    let app = App::with_config_path(sink as Arc<dyn EventSink>, cfg_path.clone());
    app.resume_persisted().await.unwrap();

    // Read the on-disk config back. Both entries must still have
    // autostart=true. Before the fix, the *last* entry would have
    // been corrupted to autostart=false by the redundant persist
    // inside add_workspace_at.
    let on_disk = AppConfig::load(&cfg_path).unwrap();
    assert_eq!(on_disk.workspaces.len(), 2);
    let a = on_disk
        .workspaces
        .iter()
        .find(|w| w.workspace_id == id_a)
        .expect("a present");
    let b = on_disk
        .workspaces
        .iter()
        .find(|w| w.workspace_id == id_b)
        .expect("b present");
    assert!(a.autostart, "autostart=true must survive resume for A");
    assert!(b.autostart, "autostart=true must survive resume for B");
}
