//! Multi-workspace lifecycle harness.
//!
//! `App` owns the registry of [`WorkspaceBinding`]s, dispatches
//! [`Command`](crate::Command)s from the GUI, runs a low-frequency
//! health-poll loop that samples each catalogue and emits
//! [`ShellEvent`](crate::ShellEvent)s on change.
//!
//! Concurrency model:
//!
//! * The `App` itself is held behind `Arc<App>` so the dispatch
//!   surface can be shared across Tauri command handlers (every
//!   handler is its own tokio task).
//! * Per-workspace mutable state (the [`WorkspaceBinding`]) lives
//!   in a `tokio::sync::Mutex<HashMap<…>>`. Each mutation holds the
//!   lock for the duration of one command; the health-poll loop
//!   takes the lock once per workspace per tick.
//! * Each running workspace owns three tokio tasks: the watcher
//!   (sync, blocking, lives in the OS thread `notify` spawned for
//!   it), the poller, and the engine reconciliation loop. Stopping
//!   a workspace closes the channels into the engine; both the
//!   poller and the engine exit on the next select tick.

use std::collections::hash_map::Entry;
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use tokio::sync::{mpsc, Mutex};
use tokio::task::JoinHandle;
use tracing::{info, warn};
use uuid::Uuid;

use zk_sync_api::Client;
use zk_sync_engine::{
    placeholder_dir, tombstone_dir, Catalogue, Engine, EngineConfig, RemotePoller, Watcher,
};

use crate::command::{Command, CommandError, CommandResult};
use crate::config::{AppConfig, WorkspaceEntry};
use crate::event::{EventSink, ShellEvent, TaskKind};
use crate::state::{SyncHealth, WorkspaceState};
use crate::summary::Summary;
use crate::tray::TrayState;

/// How often the health-poll loop wakes up to sample every
/// workspace's catalogue. One second is short enough that the tray
/// icon feels responsive but long enough that the SQLite scan
/// doesn't dominate CPU on a workspace with millions of rows.
pub const HEALTH_POLL_INTERVAL: Duration = Duration::from_secs(1);

/// Channel capacity for the watcher -> engine and poller -> engine
/// pipes. Mirrors the CLI's values so the two binaries see the
/// same backpressure characteristics under load.
const CHANNEL_CAPACITY: usize = 1024;

/// Default debounce for the [`Watcher`] in milliseconds. Mirrors
/// the CLI's hard-coded value.
const WATCHER_DEBOUNCE_MS: u64 = 250;

/// Default server-side page size for the [`RemotePoller`] catch-up
/// walk. Mirrors the CLI's `--page-size` default.
const POLLER_PAGE_SIZE: u32 = 500;

/// One workspace's runtime + persistent state.
///
/// Internal to the shell — the GUI host reads workspace data via
/// the typed [`WorkspaceState`] returned by [`App::dispatch`], not
/// by reaching into the binding directly. This keeps the public
/// surface narrow and lets us evolve the binding's internals
/// (e.g. swap `JoinHandle` for a `CancellationToken`) without
/// breaking embedders.
struct WorkspaceBinding {
    workspace_id: Uuid,
    label: String,
    root: PathBuf,
    catalogue_path: PathBuf,
    autostart: bool,

    health: SyncHealth,
    last_summary: Summary,
    last_error: Option<String>,
    last_updated: chrono::DateTime<chrono::Utc>,

    /// Open catalogue handle.
    ///
    /// Wrapped in `Option` so [`App::remove_local_cache`] can `take`
    /// the `Arc` out of the binding and drop it to close the
    /// underlying `rusqlite::Connection` *before* deleting the
    /// SQLite file. Without that, on Windows the delete would fail
    /// because the file is still locked, and on Unix any
    /// subsequent `start_sync` would reuse a stale connection
    /// pointing at the unlinked inode -- a silent data-loss bug
    /// (writes go to an inode that disappears on next restart).
    ///
    /// Invariants:
    /// * `Some(_)` after [`App::add_workspace_at`] until
    ///   [`App::remove_local_cache`] takes it out.
    /// * `None` only in the transient window between
    ///   [`App::remove_local_cache`] dropping the old handle and the
    ///   next [`App::start_sync`] re-opening at the original path.
    /// * [`App::tick_one`] treats `None` as "skip this workspace"
    ///   so the health-poll loop can't observe a partially-removed
    ///   binding.
    catalogue: Option<Arc<Mutex<Catalogue>>>,

    /// JoinHandles for the three background tasks. `None` when the
    /// workspace is in [`SyncHealth::Stopped`].
    engine_task: Option<JoinHandle<zk_sync_engine::Result<()>>>,
    poller_task: Option<JoinHandle<()>>,
    /// The OS-thread `notify::RecommendedWatcher` is non-Send-bound
    /// in some configurations; we just keep the `Watcher` wrapper
    /// alive in a tokio task whose only job is to hold the handle.
    /// Dropping the task drops the watcher.
    watcher_task: Option<JoinHandle<()>>,
}

impl WorkspaceBinding {
    fn to_state(&self) -> WorkspaceState {
        WorkspaceState {
            workspace_id: self.workspace_id,
            label: self.label.clone(),
            root: self.root.clone(),
            health: self.health,
            summary: self.last_summary,
            last_error: self.last_error.clone(),
            last_updated: self.last_updated,
        }
    }

    fn to_entry(&self) -> WorkspaceEntry {
        WorkspaceEntry {
            workspace_id: self.workspace_id,
            label: self.label.clone(),
            root: self.root.clone(),
            catalogue_path: self.catalogue_path.clone(),
            autostart: self.autostart,
        }
    }
}

/// The shell harness.
///
/// Hosts construct an `App` via [`App::new`] (or
/// [`App::with_config_path`]), then either hand the `Arc<App>` into
/// every Tauri command handler or call [`App::dispatch`] from a
/// JSON-RPC server.
pub struct App {
    workspaces: Mutex<HashMap<Uuid, WorkspaceBinding>>,
    sink: Arc<dyn EventSink>,
    config_path: Option<PathBuf>,
    /// Optional API client used to talk to the backend. `None`
    /// disables remote sync — the shell still operates the
    /// catalogue / watcher and surfaces local state, which is
    /// what every unit test in this crate exercises. Wrapped in a
    /// `OnceLock` so [`App::set_client`] can populate it post-
    /// construction without an `&mut self`; once set, the value is
    /// immutable for the rest of the shell's lifetime (a client
    /// rotation triggers a fresh `App`).
    client: std::sync::OnceLock<Arc<Client>>,
    /// Last-emitted tray state, used to suppress redundant
    /// `TrayChanged` events. Wrapped in a `Mutex` because the
    /// health-poll loop and a synchronous `dispatch` both mutate it.
    last_tray: Mutex<Option<TrayState>>,
}

/// Cheap clone-able handle the health-poll loop holds onto. Wraps
/// the `App` in `Arc` so the background task doesn't extend the
/// host's borrow lifetime.
pub type AppHandle = Arc<App>;

impl App {
    /// Build an `App` with the default in-process broadcast sink
    /// and no persistent config.
    pub fn new() -> AppHandle {
        Self::with_sink(Arc::new(crate::event::BroadcastSink::new()))
    }

    /// Inject a custom [`EventSink`]. Hosts that already own a
    /// Tauri event bus pass their wrapper here so they don't run a
    /// redundant in-process broadcast channel; tests pass a shared
    /// [`BroadcastSink`] so they can subscribe through the sink
    /// directly.
    pub fn with_sink(sink: Arc<dyn EventSink>) -> AppHandle {
        Arc::new(Self {
            workspaces: Mutex::new(HashMap::new()),
            sink,
            config_path: None,
            client: std::sync::OnceLock::new(),
            last_tray: Mutex::new(None),
        })
    }

    /// Build an `App` backed by a persistent config file. Use
    /// [`App::resume_persisted`] after construction to re-register
    /// every workspace the file knows about; the constructor is
    /// intentionally side-effect-free so unit tests can construct
    /// an `App` without touching the disk.
    pub fn with_config_path(sink: Arc<dyn EventSink>, config_path: PathBuf) -> AppHandle {
        Arc::new(Self {
            workspaces: Mutex::new(HashMap::new()),
            sink,
            config_path: Some(config_path),
            client: std::sync::OnceLock::new(),
            last_tray: Mutex::new(None),
        })
    }

    /// Attach an API client. Required before calling
    /// [`Command::StartSync`]; absent, the shell still accepts
    /// [`Command::AddWorkspace`] / [`Command::StopSync`] /
    /// [`Command::GetStatus`] etc. so a frontend can hydrate from
    /// the persisted registry before bearer tokens are loaded.
    ///
    /// Idempotent: a second call after a successful set is a no-op
    /// because `OnceLock::set` rejects the duplicate. A host that
    /// needs to rotate the bearer should build a fresh `App`
    /// rather than mutating in place — the engine tasks would
    /// otherwise be talking to a stale `Client`.
    pub fn set_client(self: &Arc<Self>, client: Arc<Client>) {
        let _ = self.client.set(client);
    }

    /// Dispatch one [`Command`] and return the typed reply.
    pub async fn dispatch(
        self: &Arc<Self>,
        cmd: Command,
    ) -> std::result::Result<CommandResult, CommandError> {
        match cmd {
            Command::AddWorkspace {
                workspace_id,
                label,
                root,
            } => self
                .add_workspace(workspace_id, label, root)
                .await
                .map(|_| CommandResult::Ok),
            Command::RemoveWorkspace { workspace_id } => self
                .remove_workspace(workspace_id)
                .await
                .map(|_| CommandResult::Ok),
            Command::RemoveLocalCache { workspace_id } => self
                .remove_local_cache(workspace_id)
                .await
                .map(|_| CommandResult::Ok),
            Command::StartSync { workspace_id } => self
                .start_sync(workspace_id)
                .await
                .map(|_| CommandResult::Ok),
            Command::StopSync { workspace_id } => self
                .stop_sync(workspace_id)
                .await
                .map(|_| CommandResult::Ok),
            Command::GetStatus { workspace_id } => {
                let map = self.workspaces.lock().await;
                let ws = map
                    .get(&workspace_id)
                    .ok_or(CommandError::NotRegistered(workspace_id))?;
                Ok(CommandResult::Status(ws.to_state()))
            }
            Command::GetTrayState => {
                let states = self.snapshot_states().await;
                Ok(CommandResult::Tray(TrayState::derive(&states)))
            }
            Command::ListWorkspaces => {
                let states = self.snapshot_states().await;
                Ok(CommandResult::Workspaces(states))
            }
        }
    }

    /// Register a workspace. Public for tests / programmatic hosts;
    /// the [`Command::AddWorkspace`] path delegates here.
    pub async fn add_workspace(
        self: &Arc<Self>,
        workspace_id: Uuid,
        label: String,
        root: PathBuf,
    ) -> std::result::Result<(), CommandError> {
        let catalogue_path = derive_catalogue_path(self.config_path.as_deref(), workspace_id);
        self.add_workspace_at(workspace_id, label, root, catalogue_path)
            .await
    }

    async fn add_workspace_at(
        self: &Arc<Self>,
        workspace_id: Uuid,
        label: String,
        root: PathBuf,
        catalogue_path: PathBuf,
    ) -> std::result::Result<(), CommandError> {
        // Hold the workspace-map lock across the existence check
        // *and* the insert so two concurrent `AddWorkspace` calls
        // for the same id can't both pass the duplicate check and
        // race past each other. The filesystem and SQLite work is
        // synchronous and fast (sub-millisecond for an existing
        // directory / fresh SQLite open) so a single critical
        // section is acceptable for the desktop concurrency this
        // shell targets.
        let mut map = self.workspaces.lock().await;
        let entry = match map.entry(workspace_id) {
            Entry::Occupied(existing) => {
                if existing.get().root == root {
                    // Idempotent re-registration; the frontend may
                    // resend AddWorkspace after a reconnect and we
                    // shouldn't churn the catalogue / autostart
                    // flag.
                    return Ok(());
                }
                return Err(CommandError::RootMismatch {
                    workspace_id,
                    existing: existing.get().root.to_string_lossy().into_owned(),
                });
            }
            Entry::Vacant(v) => v,
        };
        if let Some(parent) = catalogue_path.parent() {
            std::fs::create_dir_all(parent)
                .map_err(|e| CommandError::Other(format!("create catalogue dir: {e}")))?;
        }
        std::fs::create_dir_all(&root)
            .map_err(|e| CommandError::Other(format!("create workspace root: {e}")))?;
        let cat = Catalogue::open(&catalogue_path, workspace_id)
            .map_err(|e| CommandError::Other(format!("open catalogue: {e}")))?;
        // Seed the summary from whatever already lives in the
        // catalogue. For a freshly-created file this is a default
        // (all-zero) summary; for a resumed workspace the rows
        // accumulated over the last run hydrate the tray icon
        // immediately, without waiting for start_sync.
        let seed_summary = summary_from_catalogue(&cat, workspace_id)
            .map_err(|e| CommandError::Other(format!("seed summary: {e}")))?;
        entry.insert(WorkspaceBinding {
            workspace_id,
            label: label.clone(),
            root,
            catalogue_path,
            autostart: false,
            health: SyncHealth::Stopped,
            last_summary: seed_summary,
            last_error: None,
            last_updated: chrono::Utc::now(),
            catalogue: Some(Arc::new(Mutex::new(cat))),
            engine_task: None,
            poller_task: None,
            watcher_task: None,
        });
        drop(map);
        self.persist().await.ok();
        self.sink
            .emit(ShellEvent::WorkspaceAdded {
                workspace_id,
                label,
            })
            .await;
        self.emit_tray_if_changed().await;
        Ok(())
    }

    /// Drop a workspace. Stops the background tasks first.
    pub async fn remove_workspace(
        self: &Arc<Self>,
        workspace_id: Uuid,
    ) -> std::result::Result<(), CommandError> {
        self.stop_sync(workspace_id).await.ok();
        {
            let mut map = self.workspaces.lock().await;
            if map.remove(&workspace_id).is_none() {
                return Err(CommandError::NotRegistered(workspace_id));
            }
        }
        self.persist().await.ok();
        self.sink
            .emit(ShellEvent::WorkspaceRemoved { workspace_id })
            .await;
        self.emit_tray_if_changed().await;
        Ok(())
    }

    /// Delete a workspace's catalogue file. Requires the workspace
    /// to be stopped first.
    ///
    /// Takes the `Arc<Mutex<Catalogue>>` out of the binding before
    /// touching the filesystem so the underlying SQLite
    /// `Connection` is closed *before* `std::fs::remove_file` runs.
    /// Skipping that step left the connection live on the unlinked
    /// inode -- harmless on Unix in isolation but a Windows file-
    /// lock error in practice, and a silent data-loss bug if a
    /// subsequent `start_sync` reused the stale handle.
    pub async fn remove_local_cache(
        self: &Arc<Self>,
        workspace_id: Uuid,
    ) -> std::result::Result<(), CommandError> {
        let mut map = self.workspaces.lock().await;
        let ws = map
            .get_mut(&workspace_id)
            .ok_or(CommandError::NotRegistered(workspace_id))?;
        if ws.health.is_running() {
            return Err(CommandError::AlreadyRunning(workspace_id));
        }
        let path = ws.catalogue_path.clone();
        // Drop the binding-side Arc. Other holders (a tick_one that
        // had already cloned the Arc before we took the map lock)
        // will release their clones as soon as their SQLite scan
        // finishes; in the meantime `tick_one` itself bails on the
        // very next call because `ws.catalogue` is now `None`.
        let cat_arc = ws.catalogue.take();
        drop(map);
        // Wait a bounded amount of time for any in-flight clones to
        // drop so the rusqlite Connection finishes closing before
        // we try to delete the file. On Unix this isn't strictly
        // necessary (unlink on an open inode succeeds), but on
        // Windows the file would be locked and `remove_file` would
        // fail. 50ms x 20 tries = up to one second; well-behaved
        // ticks finish in <100ms even on a million-row catalogue.
        if let Some(arc) = cat_arc {
            let mut arc = arc;
            for _ in 0..20 {
                match Arc::try_unwrap(arc) {
                    Ok(mutex) => {
                        // Last reference released; the Connection
                        // is dropped here when `mutex` goes out of
                        // scope at the end of this match arm.
                        drop(mutex);
                        break;
                    }
                    Err(returned) => {
                        arc = returned;
                        tokio::time::sleep(Duration::from_millis(50)).await;
                    }
                }
            }
            // If we never reached strong_count==1 we still drop
            // our last clone here -- on Unix the delete below
            // proceeds regardless; on Windows the caller will see
            // the file-lock error returned by `remove_file` and
            // can retry after the in-flight tick finishes.
        }
        if path.exists() {
            std::fs::remove_file(&path)
                .map_err(|e| CommandError::Other(format!("remove catalogue: {e}")))?;
            // The WAL / SHM sidecars are SQLite's bookkeeping;
            // delete them too so a re-open starts from a clean
            // slate. `remove_file` on a missing sidecar is a
            // no-op-with-error, so we ignore the result.
            let _ = std::fs::remove_file(format!("{}-wal", path.display()));
            let _ = std::fs::remove_file(format!("{}-shm", path.display()));
        }
        Ok(())
    }

    /// Start the background sync loop for one workspace.
    ///
    /// The workspace-map lock is held for the full duration of
    /// this call (no `.await` between the existence check and the
    /// installation of the spawned `JoinHandle`s) so the previous
    /// two-phase pattern can no longer leak tasks if a concurrent
    /// `RemoveWorkspace` or second `StartSync` interleaves between
    /// phases. All the pre-spawn work -- filesystem `create_dir_all`,
    /// channel construction, `Watcher::start_with_ignore`, and the
    /// three `tokio::spawn` calls themselves -- is synchronous and
    /// completes in well under a tick, so holding the lock through
    /// it is acceptable for desktop concurrency.
    pub async fn start_sync(
        self: &Arc<Self>,
        workspace_id: Uuid,
    ) -> std::result::Result<(), CommandError> {
        let client = self.client.get().cloned().ok_or_else(|| {
            CommandError::Other(
                "shell has no API client configured; call App::set_client before StartSync".into(),
            )
        })?;

        let mut map = self.workspaces.lock().await;
        let ws = map
            .get_mut(&workspace_id)
            .ok_or(CommandError::NotRegistered(workspace_id))?;
        if ws.health.is_running() {
            return Err(CommandError::AlreadyRunning(workspace_id));
        }
        // Reopen the catalogue if `remove_local_cache` (or a
        // previous transient error) left the binding without one.
        if ws.catalogue.is_none() {
            let cat = Catalogue::open(&ws.catalogue_path, workspace_id)
                .map_err(|e| CommandError::Other(format!("reopen catalogue: {e}")))?;
            ws.catalogue = Some(Arc::new(Mutex::new(cat)));
        }
        let catalogue = ws
            .catalogue
            .as_ref()
            .expect("catalogue just ensured Some")
            .clone();
        let root = ws.root.clone();

        std::fs::create_dir_all(placeholder_dir(&root))
            .map_err(|e| CommandError::Other(format!("create placeholder dir: {e}")))?;
        std::fs::create_dir_all(tombstone_dir(&root))
            .map_err(|e| CommandError::Other(format!("create tombstone dir: {e}")))?;

        let (local_tx, local_rx) = mpsc::channel(CHANNEL_CAPACITY);
        let (remote_tx, remote_rx) = mpsc::channel(CHANNEL_CAPACITY);

        // The watcher returns a handle whose Drop unsubscribes from
        // notify. We park it inside a tokio task so a single
        // JoinHandle::abort() tears it down in lockstep with the
        // other tasks.
        let watcher_handle = Watcher::start_with_ignore(
            &root,
            Duration::from_millis(WATCHER_DEBOUNCE_MS),
            vec![placeholder_dir(&root), tombstone_dir(&root)],
            local_tx,
        )
        .map_err(|e| CommandError::Other(format!("start watcher: {e}")))?;
        let watcher_task = tokio::spawn(async move {
            // Park the watcher handle inside this task -- when the
            // task is aborted the handle drops and notify
            // unsubscribes. We can't simply `drop(watcher_handle)`
            // here because the inner notify::RecommendedWatcher
            // needs to live for as long as the task does.
            let _w = watcher_handle;
            futures::future::pending::<()>().await;
        });

        let poller = RemotePoller {
            workspace_id,
            client: client.clone(),
            catalogue: catalogue.clone(),
            page_size: POLLER_PAGE_SIZE,
        };
        // Capture two `Arc<App>` clones so the spawned poller and
        // engine tasks can call back into the app's binding map and
        // transition the workspace to `SyncHealth::Error` when they
        // exit with a failure. Without this, the `Error` variant and
        // its tray-aggregation / decay logic would be dead code
        // (the only way to reach it from the running set would be a
        // health-loop scan with no upstream signal, which the loop
        // doesn't produce).
        let app_for_poller = self.clone();
        let poller_task = tokio::spawn(async move {
            if let Err(e) = poller.run(remote_tx).await {
                let msg = format!("{e:?}");
                warn!(workspace_id = %workspace_id, "poller exited: {msg}");
                app_for_poller
                    .mark_task_failed(workspace_id, TaskKind::Poller, msg)
                    .await;
            }
        });

        let engine = Engine::new(
            EngineConfig {
                workspace_id,
                root: root.clone(),
                chunk_size: None,
            },
            client,
            catalogue,
        );
        let app_for_engine = self.clone();
        let engine_task = tokio::spawn(async move {
            let result = engine.run(local_rx, remote_rx).await;
            if let Err(ref e) = result {
                let msg = format!("{e:?}");
                warn!(workspace_id = %workspace_id, "engine exited: {msg}");
                app_for_engine
                    .mark_task_failed(workspace_id, TaskKind::Engine, msg)
                    .await;
            }
            result
        });

        ws.engine_task = Some(engine_task);
        ws.poller_task = Some(poller_task);
        ws.watcher_task = Some(watcher_task);
        ws.health = SyncHealth::Starting;
        ws.last_error = None;
        ws.last_updated = chrono::Utc::now();
        drop(map);
        self.sink
            .emit(ShellEvent::HealthChanged {
                workspace_id,
                health: SyncHealth::Starting,
                reason: None,
            })
            .await;
        info!(workspace_id = %workspace_id, "workspace sync started");
        Ok(())
    }

    /// Stop the background sync loop for one workspace.
    pub async fn stop_sync(
        self: &Arc<Self>,
        workspace_id: Uuid,
    ) -> std::result::Result<(), CommandError> {
        let (engine, poller, watcher) = {
            let mut map = self.workspaces.lock().await;
            let ws = map
                .get_mut(&workspace_id)
                .ok_or(CommandError::NotRegistered(workspace_id))?;
            if !ws.health.is_running() && ws.health != SyncHealth::Error {
                return Ok(());
            }
            ws.health = SyncHealth::Stopped;
            ws.last_updated = chrono::Utc::now();
            (
                ws.engine_task.take(),
                ws.poller_task.take(),
                ws.watcher_task.take(),
            )
        };
        // Abort all three tasks. The engine and poller cooperatively
        // exit when their channels close, but `abort` is a stronger
        // guarantee in case a task panicked and is stuck.
        if let Some(t) = engine {
            t.abort();
        }
        if let Some(t) = poller {
            t.abort();
        }
        if let Some(t) = watcher {
            t.abort();
        }
        self.sink
            .emit(ShellEvent::HealthChanged {
                workspace_id,
                health: SyncHealth::Stopped,
                reason: None,
            })
            .await;
        Ok(())
    }

    async fn snapshot_states(&self) -> Vec<WorkspaceState> {
        let map = self.workspaces.lock().await;
        map.values().map(WorkspaceBinding::to_state).collect()
    }

    async fn emit_tray_if_changed(self: &Arc<Self>) {
        let states = self.snapshot_states().await;
        let tray = TrayState::derive(&states);
        let mut last = self.last_tray.lock().await;
        if last.as_ref() == Some(&tray) {
            return;
        }
        *last = Some(tray.clone());
        drop(last);
        self.sink.emit(ShellEvent::TrayChanged { tray }).await;
    }

    /// Sample every workspace's catalogue once. Public for tests so
    /// they can drive the loop deterministically; production hosts
    /// call [`App::spawn_health_loop`].
    pub async fn tick_health(self: &Arc<Self>) {
        let ids: Vec<Uuid> = {
            let map = self.workspaces.lock().await;
            map.keys().copied().collect()
        };
        for id in ids {
            self.tick_one(id).await;
        }
        self.emit_tray_if_changed().await;
    }

    async fn tick_one(self: &Arc<Self>, workspace_id: Uuid) {
        // Compute the next summary outside the workspaces-map lock
        // so a slow SQLite read doesn't block other commands.
        // Stopped workspaces don't have an engine writing to the
        // catalogue, so the summary can't change -- skip the scan
        // entirely. This also means `remove_local_cache` (which
        // requires the workspace to be Stopped) is guaranteed not
        // to be racing with a tick_one in-flight against the same
        // workspace's catalogue.
        let catalogue = {
            let map = self.workspaces.lock().await;
            let Some(ws) = map.get(&workspace_id) else {
                return;
            };
            if ws.health == SyncHealth::Stopped {
                return;
            }
            match ws.catalogue.as_ref() {
                Some(c) => c.clone(),
                None => return,
            }
        };
        let summary = match build_summary(&catalogue, workspace_id).await {
            Ok(s) => s,
            Err(e) => {
                warn!(workspace_id = %workspace_id, "summary build failed: {e:?}");
                return;
            }
        };

        let mut summary_changed = false;
        let mut health_changed_to: Option<SyncHealth> = None;
        {
            let mut map = self.workspaces.lock().await;
            if let Some(ws) = map.get_mut(&workspace_id) {
                // Recompute next_health *inside* this second lock
                // from the live `ws.health`, not the value we read
                // before releasing the first lock. Otherwise a
                // concurrent `stop_sync` (which sets `Stopped`) or
                // `start_sync` (which sets `Starting`) that
                // interleaved between the two locks would have
                // its transition silently overwritten by us.
                let next_health = derive_health(ws.health, &summary);
                if ws.last_summary != summary {
                    summary_changed = true;
                    ws.last_summary = summary;
                }
                if ws.health != next_health {
                    ws.health = next_health;
                    health_changed_to = Some(next_health);
                }
                ws.last_updated = chrono::Utc::now();
            }
        }
        if summary_changed {
            self.sink
                .emit(ShellEvent::SummaryChanged {
                    workspace_id,
                    summary,
                })
                .await;
        }
        if let Some(h) = health_changed_to {
            self.sink
                .emit(ShellEvent::HealthChanged {
                    workspace_id,
                    health: h,
                    reason: None,
                })
                .await;
        }
    }

    /// Transition a workspace into [`SyncHealth::Error`] in response
    /// to one of its background tasks (poller or engine) exiting
    /// with a failure. Called from inside the spawned task closures
    /// in [`App::start_sync`].
    ///
    /// Contract:
    /// * Does nothing if the workspace was already user-stopped
    ///   (`Stopped`) -- the user aborted the task intentionally
    ///   and shouldn't be told a clean shutdown was a crash.
    /// * `TaskFailed` is always emitted, so a frontend log captures
    ///   every failure regardless of prior health (e.g. a poller
    ///   crash after an engine crash still surfaces both messages).
    /// * `HealthChanged{Error}` and the tray recompute are only
    ///   emitted on the *first* transition into `Error`; further
    ///   failures from sibling tasks update `last_error` silently
    ///   so the event stream isn't spammed.
    pub async fn mark_task_failed(
        self: Arc<Self>,
        workspace_id: Uuid,
        task: TaskKind,
        message: String,
    ) {
        let should_emit_health;
        {
            let mut map = self.workspaces.lock().await;
            let Some(ws) = map.get_mut(&workspace_id) else {
                // Workspace was removed while we were in flight;
                // nothing to update. Still emit TaskFailed below so
                // the frontend log shows the failure that triggered
                // the removal (e.g. the user clicking Remove after
                // seeing the engine crash).
                drop(map);
                self.sink
                    .emit(ShellEvent::TaskFailed {
                        workspace_id,
                        task,
                        message,
                    })
                    .await;
                return;
            };
            if ws.health == SyncHealth::Stopped {
                return;
            }
            should_emit_health = ws.health != SyncHealth::Error;
            ws.health = SyncHealth::Error;
            ws.last_error = Some(message.clone());
            ws.last_updated = chrono::Utc::now();
        }
        // Per `ShellEvent::TaskFailed` doc: emit *before* the
        // HealthChanged so a frontend log captures the underlying
        // reason in chronological order.
        self.sink
            .emit(ShellEvent::TaskFailed {
                workspace_id,
                task,
                message: message.clone(),
            })
            .await;
        if should_emit_health {
            self.sink
                .emit(ShellEvent::HealthChanged {
                    workspace_id,
                    health: SyncHealth::Error,
                    reason: Some(message),
                })
                .await;
            self.emit_tray_if_changed().await;
        }
    }

    /// Spawn the health-poll loop. Returns a [`JoinHandle`] the
    /// host can abort on shutdown.
    pub fn spawn_health_loop(self: &Arc<Self>) -> JoinHandle<()> {
        let app = self.clone();
        tokio::spawn(async move {
            let mut interval = tokio::time::interval(HEALTH_POLL_INTERVAL);
            interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            loop {
                interval.tick().await;
                app.tick_health().await;
            }
        })
    }

    /// Persist the workspace registry to the config file (if any).
    async fn persist(&self) -> crate::Result<()> {
        let Some(path) = self.config_path.as_ref() else {
            return Ok(());
        };
        let entries: Vec<WorkspaceEntry> = {
            let map = self.workspaces.lock().await;
            map.values().map(WorkspaceBinding::to_entry).collect()
        };
        let cfg = AppConfig {
            version: 1,
            workspaces: entries,
        };
        cfg.save(path)
    }

    /// Re-register every workspace listed in the persisted config.
    /// Does **not** start them; the host calls
    /// [`Command::StartSync`] per workspace (or, for the autostart
    /// subset, iterates `ListWorkspaces` and starts the ones with
    /// `autostart=true`).
    pub async fn resume_persisted(self: &Arc<Self>) -> crate::Result<()> {
        let Some(path) = self.config_path.clone() else {
            return Ok(());
        };
        let cfg = AppConfig::load(&path)?;
        for entry in cfg.workspaces {
            self.add_workspace_at(
                entry.workspace_id,
                entry.label,
                entry.root,
                entry.catalogue_path,
            )
            .await
            .map_err(|e| crate::ShellError::Other(format!("resume {e}")))?;
            if entry.autostart {
                let mut map = self.workspaces.lock().await;
                if let Some(ws) = map.get_mut(&entry.workspace_id) {
                    ws.autostart = true;
                }
            }
        }
        // `add_workspace_at` persists each entry as it is inserted, but it
        // always writes `autostart=false` (the just-constructed binding has
        // not yet been restored). The autostart flag is restored above,
        // in-memory only. Persist once at the end so the on-disk config
        // reflects the restored autostart flags for every workspace —
        // otherwise a crash before the next config-touching command would
        // silently clear autostart for the *last* workspace processed
        // (every prior workspace happens to be corrected by the next
        // iteration's `add_workspace_at`-driven persist).
        self.persist().await?;
        Ok(())
    }
}

/// Read every row in the catalogue, fold them into a [`Summary`],
/// then read the workspace's last-applied cursor. Synchronous so
/// callers that already hold the catalogue (e.g. the one-shot seed
/// scan in `add_workspace_at`) don't need to async-lock it.
fn summary_from_catalogue(cat: &Catalogue, workspace_id: Uuid) -> zk_sync_engine::Result<Summary> {
    let mut s = Summary::default();
    for rec in cat.list_all()? {
        s.accumulate(rec.status, rec.size_bytes);
    }
    s.cursor = cat.get_cursor(workspace_id)?;
    Ok(s)
}

/// Async wrapper around [`summary_from_catalogue`] for callers that
/// only hold the shared `Arc<Mutex<Catalogue>>` (the health-poll
/// loop is the only such caller today).
async fn build_summary(
    catalogue: &Arc<Mutex<Catalogue>>,
    workspace_id: Uuid,
) -> zk_sync_engine::Result<Summary> {
    let cat = catalogue.lock().await;
    summary_from_catalogue(&cat, workspace_id)
}

/// Pure helper: given the previous health and a fresh summary,
/// decide what health to report next. Broken out so the unit tests
/// can pin the state machine without spinning up a real engine.
fn derive_health(prev: SyncHealth, summary: &Summary) -> SyncHealth {
    match prev {
        // Once the user has stopped a workspace we don't reanimate
        // it from a poll tick. The Stopped -> running transition
        // is always driven by `start_sync`.
        SyncHealth::Stopped => SyncHealth::Stopped,
        // An error is sticky until either StartSync is re-invoked
        // (which clears `last_error`) or the poll loop sees a
        // clean summary -- in the latter case we *do* recover to
        // Idle/Syncing/Conflict.
        SyncHealth::Error => derive_active_health(summary),
        SyncHealth::Starting | SyncHealth::Idle | SyncHealth::Syncing | SyncHealth::Conflict => {
            derive_active_health(summary)
        }
    }
}

fn derive_active_health(summary: &Summary) -> SyncHealth {
    if summary.conflict > 0 {
        SyncHealth::Conflict
    } else if summary.in_flight > 0 {
        SyncHealth::Syncing
    } else if summary.pending() > 0 {
        // Anything pending that isn't already in-flight is still
        // work to do; treat as syncing so the tray icon spins.
        SyncHealth::Syncing
    } else {
        SyncHealth::Idle
    }
}

/// Default catalogue path: a sibling of the app config file (or,
/// for in-memory test shells, the system temp dir). Kept centralised
/// so a future "user picks a custom location" flow has one place to
/// override.
fn derive_catalogue_path(config_path: Option<&Path>, workspace_id: Uuid) -> PathBuf {
    let base = match config_path {
        Some(p) => p
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(|| std::env::temp_dir().join(format!("zk-sync-shell-{workspace_id}"))),
        None => std::env::temp_dir().join(format!("zk-sync-shell-{workspace_id}")),
    };
    base.join(format!("{workspace_id}.db"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn derive_health_keeps_stopped() {
        assert_eq!(
            derive_health(SyncHealth::Stopped, &Summary::default()),
            SyncHealth::Stopped
        );
        // Even with pending work, a stopped workspace stays
        // stopped -- the user said "don't sync".
        let mut s = Summary::default();
        s.accumulate(zk_sync_engine::SyncStatus::LocalDirty, 1);
        assert_eq!(derive_health(SyncHealth::Stopped, &s), SyncHealth::Stopped);
    }

    #[test]
    fn derive_health_conflict_dominates_in_flight() {
        let mut s = Summary::default();
        s.accumulate(zk_sync_engine::SyncStatus::Conflict, 1);
        s.accumulate(zk_sync_engine::SyncStatus::InFlight, 1);
        assert_eq!(derive_health(SyncHealth::Idle, &s), SyncHealth::Conflict);
    }

    #[test]
    fn derive_health_in_flight_renders_syncing() {
        let mut s = Summary::default();
        s.accumulate(zk_sync_engine::SyncStatus::InFlight, 1);
        assert_eq!(derive_health(SyncHealth::Idle, &s), SyncHealth::Syncing);
    }

    #[test]
    fn derive_health_pending_without_in_flight_still_syncs() {
        // A row that's LocalDirty but not yet picked up is still
        // work to do; tray should not show Idle.
        let mut s = Summary::default();
        s.accumulate(zk_sync_engine::SyncStatus::LocalDirty, 1);
        assert_eq!(derive_health(SyncHealth::Idle, &s), SyncHealth::Syncing);
    }

    #[test]
    fn derive_health_error_recovers_on_clean_summary() {
        // Once the network heals and the summary settles, an
        // Error state must transition to Idle on its own.
        let s = Summary::default();
        assert_eq!(derive_health(SyncHealth::Error, &s), SyncHealth::Idle);
    }

    /// Build a minimal `WorkspaceBinding` directly in the workspaces
    /// map so the unit test below can drive `mark_task_failed`
    /// against a binding that is in a *running* state without
    /// having to spin up a real `Client`. The integration tests in
    /// `tests/dispatch_integration.rs` exercise the public API
    /// (`AddWorkspace` + dispatch), but those leave the binding in
    /// `Stopped` because no real client is available to take it
    /// past `Starting`. The error-transition path is what we need
    /// to pin here, so we reach inside the private binding type.
    async fn seed_running_binding(app: &Arc<App>, workspace_id: Uuid, initial: SyncHealth) {
        let mut map = app.workspaces.lock().await;
        map.insert(
            workspace_id,
            WorkspaceBinding {
                workspace_id,
                label: "seeded".into(),
                root: std::path::PathBuf::from("/tmp/seeded"),
                catalogue_path: std::path::PathBuf::from("/tmp/seeded.db"),
                autostart: false,
                health: initial,
                last_summary: Summary::default(),
                last_error: None,
                last_updated: chrono::Utc::now(),
                catalogue: None,
                engine_task: None,
                poller_task: None,
                watcher_task: None,
            },
        );
    }

    #[tokio::test]
    async fn mark_task_failed_transitions_running_workspace_to_error() {
        // Regression guard for `SyncHealth::Error` having no
        // producer in the running set: the poller / engine spawn
        // closures previously emitted `TaskFailed` but never updated
        // the binding, so the `Error` variant was dead code as far
        // as the state machine was concerned. `mark_task_failed`
        // is the single entry point that:
        //   (a) emits `ShellEvent::TaskFailed`,
        //   (b) flips the binding's `health` to `Error`,
        //   (c) populates `last_error`,
        //   (d) emits `HealthChanged{Error}` on the first
        //       transition only, suppressing subsequent
        //       `HealthChanged` for sibling failures in the same
        //       Error lifecycle (the `TaskFailed` event still
        //       fires so the frontend log captures every failure).
        let sink = Arc::new(crate::event::BroadcastSink::new());
        let app = App::with_sink(sink.clone() as Arc<dyn EventSink>);
        let id = Uuid::new_v4();
        seed_running_binding(&app, id, SyncHealth::Idle).await;

        let mut rx = sink.subscribe();
        app.clone()
            .mark_task_failed(id, TaskKind::Engine, "engine: synthetic crash".into())
            .await;

        // First event: TaskFailed.
        let first = rx.try_recv().expect("TaskFailed should be emitted");
        match first {
            ShellEvent::TaskFailed {
                workspace_id,
                task,
                message,
            } => {
                assert_eq!(workspace_id, id);
                assert_eq!(task, TaskKind::Engine);
                assert_eq!(message, "engine: synthetic crash");
            }
            other => panic!("expected TaskFailed first, got {other:?}"),
        }

        // Second event: HealthChanged{Error, reason=msg}.
        let second = rx.try_recv().expect("HealthChanged{Error} should follow");
        match second {
            ShellEvent::HealthChanged {
                workspace_id,
                health,
                reason,
            } => {
                assert_eq!(workspace_id, id);
                assert_eq!(health, SyncHealth::Error);
                assert_eq!(reason.as_deref(), Some("engine: synthetic crash"));
            }
            other => panic!("expected HealthChanged{{Error}}, got {other:?}"),
        }

        // Third event: TrayChanged (the only running workspace
        // moved from Idle to Error, so the tray aggregate flips).
        let third = rx.try_recv().expect("TrayChanged should follow");
        assert!(matches!(third, ShellEvent::TrayChanged { .. }));

        // Binding state pinned.
        {
            let map = app.workspaces.lock().await;
            let ws = map.get(&id).expect("workspace present");
            assert_eq!(ws.health, SyncHealth::Error);
            assert_eq!(ws.last_error.as_deref(), Some("engine: synthetic crash"));
        }

        // Sibling failure (poller crashes right after engine):
        // must emit TaskFailed but NOT a second HealthChanged.
        app.clone()
            .mark_task_failed(id, TaskKind::Poller, "poller: synthetic crash".into())
            .await;
        let fourth = rx.try_recv().expect("sibling TaskFailed should fire");
        match fourth {
            ShellEvent::TaskFailed { task, message, .. } => {
                assert_eq!(task, TaskKind::Poller);
                assert_eq!(message, "poller: synthetic crash");
            }
            other => panic!("expected sibling TaskFailed, got {other:?}"),
        }
        // No further HealthChanged / TrayChanged for the in-Error
        // lifecycle.
        assert!(
            rx.try_recv().is_err(),
            "no further events for in-Error workspace"
        );

        // last_error reflects the most recent failure.
        {
            let map = app.workspaces.lock().await;
            let ws = map.get(&id).expect("workspace present");
            assert_eq!(ws.last_error.as_deref(), Some("poller: synthetic crash"));
        }
    }

    #[tokio::test]
    async fn mark_task_failed_for_unknown_workspace_still_emits_task_failed() {
        // The workspace was removed between the task crashing and
        // the closure calling back into the app. We still want a
        // log line for the failure (it explains why the task
        // exited) but there's no binding to flip into Error.
        let sink = Arc::new(crate::event::BroadcastSink::new());
        let app = App::with_sink(sink.clone() as Arc<dyn EventSink>);
        let id = Uuid::new_v4();
        let mut rx = sink.subscribe();
        app.clone()
            .mark_task_failed(id, TaskKind::Engine, "engine: ghost".into())
            .await;
        let first = rx.try_recv().expect("TaskFailed should still fire");
        assert!(matches!(
            first,
            ShellEvent::TaskFailed {
                task: TaskKind::Engine,
                ..
            }
        ));
        assert!(
            rx.try_recv().is_err(),
            "nothing else fires for an unknown workspace"
        );
    }
}
