//! `zk-sync` — headless ZK Drive desktop sync daemon.
//!
//! Subcommands:
//!
//!   * `zk-sync login --base-url …`        — kicks off OAuth2 PKCE.
//!   * `zk-sync status --workspace …`      — prints current cursor + catalogue health.
//!   * `zk-sync run    --workspace …`      — long-running sync loop.
//!
//! The Tauri shell links the same crates directly and exposes a tray
//! UI on top. This binary is what CI builds against (it is the
//! integration surface that exercises every crate end-to-end).

use std::path::PathBuf;
use std::sync::Arc;

use clap::{Parser, Subcommand};
use tokio::sync::{mpsc, Mutex};
use tracing::info;
use uuid::Uuid;

use zk_sync_api::{Bearer, Client};
use zk_sync_engine::{
    evict_to_quota, placeholder_dir, tombstone_dir, Catalogue, ConnectivityState, Engine,
    EngineConfig, EvictionTrigger, OnlineState, RemotePoller, StatusSnapshot, Watcher,
};

#[derive(Parser)]
#[command(name = "zk-sync", version, about = "ZK Drive desktop sync daemon")]
struct Cli {
    /// ZK Drive backend base URL, e.g. https://drive.uney.com
    #[arg(long, env = "ZK_DRIVE_BASE_URL")]
    base_url: String,
    /// Path to the catalogue SQLite database.
    #[arg(
        long,
        env = "ZK_DRIVE_CATALOGUE_PATH",
        default_value = "~/.zk-sync/catalogue.db"
    )]
    catalogue: String,
    /// File containing the bearer token for the API client (one
    /// line, optional trailing newline). Preferred over passing the
    /// token via the [`ZK_DRIVE_BEARER`] environment variable in
    /// shared environments — the file's permissions can be locked
    /// to mode 0600 whereas environment variables can leak into
    /// child processes via `/proc/<pid>/environ`. Mutually exclusive
    /// with `ZK_DRIVE_BEARER`; if both are set the file wins so a
    /// stale env var in the shell can't override a freshly-rotated
    /// token on disk.
    ///
    /// We deliberately do NOT accept the raw token as a CLI value
    /// (e.g. `--bearer <TOKEN>`). On Linux the kernel exposes every
    /// process's argv via `/proc/<pid>/cmdline` to every user on the
    /// host, so a flag-passed secret is visible to `ps`, `htop`,
    /// monitoring agents, and shell history. File / env-var paths
    /// avoid that exposure surface.
    #[arg(long, env = "ZK_DRIVE_BEARER_FILE")]
    bearer_file: Option<PathBuf>,

    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Print the engine's local state for a workspace.
    Status {
        #[arg(long)]
        workspace: Uuid,
        /// Emit JSON instead of the human-readable summary so the
        /// Tauri shell (and curious operators) can pipe the output
        /// through `jq` without reparsing prose.
        #[arg(long)]
        json: bool,
    },
    /// Run the long-lived sync loop until SIGINT.
    Run {
        #[arg(long)]
        workspace: Uuid,
        #[arg(long)]
        root: PathBuf,
        /// Server-side page size for the catch-up cursor walk.
        #[arg(long, default_value = "500")]
        page_size: u32,
        /// Maximum bytes the local cache may occupy. When the cache
        /// exceeds this size the engine triggers LRU eviction on
        /// non-pinned `UpToDate` rows. Omit to disable eviction.
        #[arg(long)]
        disk_quota_bytes: Option<u64>,
    },
    /// Mark a file for offline retention (excluded from LRU eviction).
    Pin {
        #[arg(long)]
        workspace: Uuid,
        /// Remote file id (UUID) to pin. The CLI takes the remote
        /// id rather than the local path so an operator can pin a
        /// file that has been evicted (no longer on disk) -- a
        /// pin-by-path-only API would require the file to be
        /// materialised first, defeating the purpose for a user
        /// who knows the id from the web UI.
        #[arg(long)]
        file: Uuid,
    },
    /// Clear the offline-pin bit on a file.
    Unpin {
        #[arg(long)]
        workspace: Uuid,
        #[arg(long)]
        file: Uuid,
    },
    /// Run a one-shot LRU eviction pass. Used by the Tauri tray
    /// "Free up space" button and by `cron` setups that want to
    /// keep the cache trimmed without running the long-lived `run`
    /// loop.
    Evict {
        #[arg(long)]
        workspace: Uuid,
        /// Bytes the cache must be brought at or below. Use `0` to
        /// evict every non-pinned `UpToDate` row.
        #[arg(long)]
        quota_bytes: u64,
        #[arg(long)]
        root: PathBuf,
    },
}

/// Expand a leading `~/` into the user's home directory.
///
/// The default catalogue path is `~/.zk-sync/catalogue.db`. On
/// Unix `$HOME` is the canonical home; on Windows we fall back to
/// `%USERPROFILE%` and finally to `%HOMEDRIVE%%HOMEPATH%` so the
/// CLI keeps producing a sane catalogue location once the Tauri
/// shell ships on Windows. If none of the variables are set we
/// pass through the raw input so the user gets a clear error
/// rather than a literal `~` directory in the working directory.
fn expand_tilde(path: &str) -> PathBuf {
    // Handle the three meaningful inputs: a bare `~`, `~/...`, and
    // `~\...` (PowerShell). Anything else passes through untouched
    // -- e.g. `~user/...` is reserved for future per-user expansion
    // and right now should surface as a clear "file not found"
    // instead of being silently mangled.
    if path == "~" {
        return home_dir().unwrap_or_else(|| PathBuf::from(path));
    }
    let stripped = match path.strip_prefix("~/") {
        Some(s) => s,
        None => match path.strip_prefix("~\\") {
            Some(s) => s,
            None => return PathBuf::from(path),
        },
    };
    if let Some(home) = home_dir() {
        return home.join(stripped);
    }
    PathBuf::from(path)
}

/// Resolve the bearer token from (in order of precedence):
///
/// 1. A `--bearer-file` argument (or the `ZK_DRIVE_BEARER_FILE`
///    environment variable that clap exposes via the same arg).
///    The file is read in full, trimmed of trailing whitespace,
///    and the bytes are interpreted as UTF-8. Permission bits are
///    NOT validated by this binary — operators are expected to
///    `chmod 600` the file themselves; the keychain-backed `login`
///    subcommand is the path for hands-off provisioning.
/// 2. The `ZK_DRIVE_BEARER` environment variable, kept for CI /
///    automation workflows that already inject secrets into the
///    process environment via the orchestrator (GitHub Actions,
///    `kubectl exec -e`, systemd `EnvironmentFile=`, etc.). Those
///    secrets are NOT exposed to other users on the host because
///    `/proc/<pid>/environ` is mode-0400.
///
/// Returns an error if neither source supplies a non-empty value.
/// We never accept the token as a positional or flag argument —
/// see the doc-comment on [`Cli::bearer_file`] for the rationale.
fn load_bearer(bearer_file: Option<&std::path::Path>) -> anyhow::Result<String> {
    if let Some(p) = bearer_file {
        let raw = std::fs::read_to_string(p)
            .map_err(|e| anyhow::anyhow!("failed to read bearer file {}: {e}", p.display()))?;
        let trimmed = raw.trim().to_string();
        if trimmed.is_empty() {
            return Err(anyhow::anyhow!(
                "bearer file {} is empty after trimming whitespace",
                p.display()
            ));
        }
        return Ok(trimmed);
    }
    match std::env::var("ZK_DRIVE_BEARER") {
        Ok(v) if !v.trim().is_empty() => Ok(v.trim().to_string()),
        _ => Err(anyhow::anyhow!(
            "no bearer token supplied; pass --bearer-file <PATH> or set ZK_DRIVE_BEARER"
        )),
    }
}

fn home_dir() -> Option<PathBuf> {
    if let Some(h) = std::env::var_os("HOME") {
        return Some(PathBuf::from(h));
    }
    if let Some(h) = std::env::var_os("USERPROFILE") {
        return Some(PathBuf::from(h));
    }
    match (std::env::var_os("HOMEDRIVE"), std::env::var_os("HOMEPATH")) {
        (Some(drive), Some(path)) => {
            let mut p = PathBuf::from(drive);
            p.push(path);
            Some(p)
        }
        _ => None,
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
        )
        .init();

    let args = Cli::parse();
    let catalogue_path = expand_tilde(&args.catalogue);
    if let Some(parent) = catalogue_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    // The catalogue is bound to one workspace for its lifetime so the
    // `files` table (keyed by remote_file_id / local_path only) can
    // never accidentally co-mingle rows from a second workspace if
    // the user reuses `--catalogue` across `--workspace` flags. Pull
    // the workspace out of the subcommand here and surface it to the
    // catalogue opener.
    let workspace_id = match &args.cmd {
        Cmd::Status { workspace, .. } => *workspace,
        Cmd::Run { workspace, .. } => *workspace,
        Cmd::Pin { workspace, .. } => *workspace,
        Cmd::Unpin { workspace, .. } => *workspace,
        Cmd::Evict { workspace, .. } => *workspace,
    };
    let cat = Catalogue::open(&catalogue_path, workspace_id)?;
    let catalogue = Arc::new(Mutex::new(cat));

    // Bearer / HTTP client construction is deferred into the
    // network-touching arms only. `Status` is a purely local query
    // (it reads the SQLite catalogue and prints a cursor + row
    // count) so requiring a token there is a usability foot-gun --
    // operators want to be able to `zk-sync status` on a box that
    // has lost network credentials to diagnose what the agent
    // thought the last-known state was.
    match args.cmd {
        Cmd::Status { workspace, json } => {
            let c = catalogue.lock().await;
            let cursor = c.get_cursor(workspace)?;
            // The engine isn't running here so we can't surface a
            // live connectivity flag; report `Unknown` instead of
            // lying about online state. The status snapshot is
            // otherwise authoritative (everything else comes from
            // SQLite).
            let snap = StatusSnapshot::from_catalogue(&c, None, ConnectivityState::Unknown)?;
            if json {
                // Include the cursor in a JSON envelope alongside
                // the snapshot so tools that diff `status --json`
                // can detect new catch-up progress.
                let envelope = serde_json::json!({
                    "cursor": cursor,
                    "snapshot": snap,
                });
                println!("{}", serde_json::to_string_pretty(&envelope)?);
            } else {
                println!(
                    "workspace={workspace} cursor={cursor} cached_bytes={} pinned={} \
                     up_to_date={} local_dirty={} remote_dirty={} conflict={} evicted={} \
                     connectivity={}",
                    snap.cached_bytes,
                    snap.pinned_count,
                    snap.status_counts.up_to_date,
                    snap.status_counts.local_dirty,
                    snap.status_counts.remote_dirty,
                    snap.status_counts.conflict,
                    snap.status_counts.evicted,
                    match snap.connectivity {
                        zk_sync_engine::ConnectivityStateOwned::Online => "online",
                        zk_sync_engine::ConnectivityStateOwned::Offline => "offline",
                        zk_sync_engine::ConnectivityStateOwned::Unknown => "unknown",
                    },
                );
            }
        }
        Cmd::Pin { workspace: _, file } => {
            let mut c = catalogue.lock().await;
            match c.set_pinned(file, true)? {
                Some(prior) => {
                    if prior {
                        info!(file = %file, "file was already pinned; no-op");
                    } else {
                        info!(file = %file, "file pinned");
                    }
                }
                None => {
                    eprintln!("file {file} is not tracked by this catalogue");
                    std::process::exit(2);
                }
            }
        }
        Cmd::Unpin { workspace: _, file } => {
            let mut c = catalogue.lock().await;
            match c.set_pinned(file, false)? {
                Some(prior) => {
                    if !prior {
                        info!(file = %file, "file was already unpinned; no-op");
                    } else {
                        info!(file = %file, "file unpinned; eligible for eviction");
                    }
                }
                None => {
                    eprintln!("file {file} is not tracked by this catalogue");
                    std::process::exit(2);
                }
            }
        }
        Cmd::Evict {
            workspace: _,
            quota_bytes,
            root,
        } => {
            let mut c = catalogue.lock().await;
            let report = evict_to_quota(&mut c, &root, quota_bytes, EvictionTrigger::Manual)?;
            println!(
                "evicted={} reclaimed_bytes={} final_cached_bytes={} unreachable={}",
                report.evicted_count,
                report.bytes_reclaimed,
                report.final_cached_bytes,
                report.quota_unreachable,
            );
            if report.quota_unreachable {
                // Surface a non-zero exit so a cron-driven run
                // shows up in the operator's email; the agent
                // can't free more space on its own and the user
                // needs to either raise the quota or unpin rows.
                std::process::exit(3);
            }
        }
        Cmd::Run {
            workspace,
            root,
            page_size,
            disk_quota_bytes,
        } => {
            let bearer = load_bearer(args.bearer_file.as_deref())?;
            let client = Arc::new(
                Client::builder(&args.base_url)
                    .bearer(Bearer(bearer))
                    .build()?,
            );
            std::fs::create_dir_all(&root)?;
            // Pre-create the hidden engine-owned directories so the
            // watcher resolves them canonically on platforms where
            // notify emits canonical paths only.
            std::fs::create_dir_all(placeholder_dir(&root))?;
            std::fs::create_dir_all(tombstone_dir(&root))?;
            let (local_tx, local_rx) = mpsc::channel(1024);
            let (remote_tx, remote_rx) = mpsc::channel(1024);
            let _watcher = Watcher::start_with_ignore(
                &root,
                std::time::Duration::from_millis(250),
                vec![placeholder_dir(&root), tombstone_dir(&root)],
                local_tx,
            )?;

            // A single connectivity flag is shared between the
            // poller (writer) and the engine (reader) so the
            // future upload loop can back off cleanly when the
            // network drops.
            let online = OnlineState::new();
            let poller = RemotePoller {
                workspace_id: workspace,
                client: client.clone(),
                catalogue: catalogue.clone(),
                page_size,
                online: online.clone(),
            };
            tokio::spawn(async move {
                if let Err(e) = poller.run(remote_tx).await {
                    tracing::error!("poller exited: {e:?}");
                }
            });

            let engine = Engine::new(
                EngineConfig {
                    workspace_id: workspace,
                    root,
                    chunk_size: None,
                    disk_quota_bytes,
                },
                client,
                catalogue,
            )
            .with_online(online);
            info!("zk-sync started");
            engine.run(local_rx, remote_rx).await?;
        }
    }
    Ok(())
}
