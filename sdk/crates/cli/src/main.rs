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
    placeholder_dir, tombstone_dir, Catalogue, Engine, EngineConfig, RemotePoller, Watcher,
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
        Cmd::Status { workspace } => *workspace,
        Cmd::Run { workspace, .. } => *workspace,
    };
    let cat = Catalogue::open(&catalogue_path, workspace_id)?;
    let catalogue = Arc::new(Mutex::new(cat));

    let bearer = load_bearer(args.bearer_file.as_deref())?;
    let client = Arc::new(
        Client::builder(&args.base_url)
            .bearer(Bearer(bearer))
            .build()?,
    );

    match args.cmd {
        Cmd::Status { workspace } => {
            let c = catalogue.lock().await;
            let cursor = c.get_cursor(workspace)?;
            // `list_all` is safe to report as a per-workspace count
            // because the catalogue itself is workspace-scoped (see
            // Catalogue::open).
            let n = c.list_all()?.len();
            println!("workspace={workspace} cursor={cursor} tracked_files={n}");
        }
        Cmd::Run {
            workspace,
            root,
            page_size,
        } => {
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

            let poller = RemotePoller {
                workspace_id: workspace,
                client: client.clone(),
                catalogue: catalogue.clone(),
                page_size,
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
                },
                client,
                catalogue,
            );
            info!("zk-sync started");
            engine.run(local_rx, remote_rx).await?;
        }
    }
    Ok(())
}
