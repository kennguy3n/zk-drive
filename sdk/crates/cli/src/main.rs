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
use zk_sync_engine::{Catalogue, Engine, EngineConfig, RemotePoller, Watcher};

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
    /// Bearer token for the API client.
    #[arg(long, env = "ZK_DRIVE_BEARER")]
    bearer: String,

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

fn expand_tilde(path: &str) -> PathBuf {
    if let Some(stripped) = path.strip_prefix("~/") {
        if let Some(home) = std::env::var_os("HOME") {
            return PathBuf::from(home).join(stripped);
        }
    }
    PathBuf::from(path)
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
    let cat = Catalogue::open(&catalogue_path)?;
    let catalogue = Arc::new(Mutex::new(cat));

    let client = Arc::new(
        Client::builder(&args.base_url)
            .bearer(Bearer(args.bearer.clone()))
            .build()?,
    );

    match args.cmd {
        Cmd::Status { workspace } => {
            let c = catalogue.lock().await;
            let cursor = c.get_cursor(workspace)?;
            let n = c.list_all()?.len();
            println!("workspace={workspace} cursor={cursor} tracked_files={n}");
        }
        Cmd::Run {
            workspace,
            root,
            page_size,
        } => {
            std::fs::create_dir_all(&root)?;
            let (local_tx, local_rx) = mpsc::channel(1024);
            let (remote_tx, remote_rx) = mpsc::channel(1024);
            let _watcher = Watcher::start(&root, std::time::Duration::from_millis(250), local_tx)?;

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
