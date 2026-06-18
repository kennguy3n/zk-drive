# `zk-sync-sdk` — ZK Drive desktop sync SDK

A Rust workspace that provides the shared core for the ZK Drive
desktop sync daemon, the future Tauri tray UI, and any native
clients we layer on top (e.g. mobile via UniFFI). It is split into
six focused crates so each module can be vendored independently
into the Tauri / Electron shell, the headless agent, or the
eventual collab editor.

| Crate             | Purpose                                                                                                                                                                  |
| ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `zk-sync-crypto`  | XChaCha20-Poly1305 envelope that is byte-for-byte compatible with `zk-object-fabric/encryption/client_sdk` (Go). Per-chunk AAD, convergent nonce option.                 |
| `zk-sync-api`     | Typed HTTP / WebSocket client against ZK Drive: presigned upload/download, workspace change feed, folder & file metadata.                              |
| `zk-sync-auth`    | OAuth2 PKCE (RFC 7636) + a coalescing `TokenSource` backed by the OS keychain (`keyring` crate).                                                                         |
| `zk-sync-engine`  | Local SQLite catalogue, cross-platform filesystem watcher (`notify`), remote poller (cursor + WebSocket), reconciliation loop, last-writer-wins conflict policy.         |
| `zk-sync-shell`   | Embedding-friendly multi-workspace harness. Owns the registry of bindings, persists it as a JSON sidecar, exposes a serde-shaped `Command` / `ShellEvent` IPC surface, derives a cross-workspace tray state. Any GUI host (Tauri, Electron, native) drives the engine through this crate. |
| `zk-sync-cli`     | `zk-sync` daemon binary. Headless agent that wires the above crates together; useful for servers and end-to-end CI smoke tests.                                          |

## Cross-language interop

The crypto crate is validated against ground-truth ciphertext
produced by the Go SDK in `zk-object-fabric/encryption/client_sdk`.
The 5 test vectors live in
`crates/crypto/testdata/envelope-vectors.json` and assert both
directions:

1. Rust encrypt → matches Go ciphertext byte-for-byte.
2. Rust decrypt of Go ciphertext → recovers the original plaintext.

If you change the wire format, regenerate the vectors with
`go run ./genvectors` against the Go SDK and check the new file in
along with the change.

## Layout

```
sdk/
├─ Cargo.toml                       # workspace manifest
├─ crates/
│  ├─ crypto/                       # zk-sync-crypto
│  │  ├─ src/{lib,error,envelope,aad,stream}.rs
│  │  ├─ testdata/envelope-vectors.json
│  │  └─ tests/go_compat.rs         # cross-language fixture test
│  ├─ api-client/                   # zk-sync-api
│  │  └─ src/{lib,error,transport,fs,storage,changefeed}.rs
│  ├─ auth/                         # zk-sync-auth
│  │  └─ src/{lib,pkce,token,store}.rs
│  ├─ sync-engine/                  # zk-sync-engine
│  │  └─ src/{lib,catalogue,events,hash,watcher,poller,conflict,engine}.rs
│  ├─ desktop-shell/                # zk-sync-shell
│  │  ├─ src/{lib,app,command,config,event,state,summary,tray}.rs
│  │  └─ tests/dispatch_integration.rs
│  └─ cli/                          # zk-sync-cli
│     └─ src/main.rs
```

## Driving the shell from a GUI host

The Tauri / Electron host owns the window + tray UI; the shell
owns the engine wiring. A minimal host looks like this:

```rust
use std::sync::Arc;
use zk_sync_api::{Bearer, Client};
use zk_sync_shell::{App, BroadcastSink, Command, EventSink, ShellEvent};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let sink = Arc::new(BroadcastSink::new());
    let app = App::with_config_path(
        sink.clone() as Arc<dyn EventSink>,
        dirs::config_dir().unwrap().join("zk-sync/app.json"),
    );
    app.resume_persisted().await?;

    let client = Arc::new(
        Client::builder("https://drive.example.com")
            .bearer(Bearer(std::env::var("ZK_DRIVE_BEARER")?))
            .build()?,
    );
    app.set_client(client);

    let _health_loop = app.spawn_health_loop();

    // Forward shell events to the GUI's own event bus.
    let mut rx = sink.subscribe();
    tokio::spawn(async move {
        while let Ok(ev) = rx.recv().await {
            // Tauri: window.emit("sync", &ev).unwrap();
            // Electron: webContents.send("sync", &ev);
            tracing::info!("shell event: {ev:?}");
        }
    });

    // Dispatch user actions from the GUI's command handlers.
    app.dispatch(Command::AddWorkspace {
        workspace_id: uuid::Uuid::new_v4(),
        label: "Acme".into(),
        root: "/Users/me/Acme".into(),
    })
    .await?;

    tokio::signal::ctrl_c().await?;
    Ok(())
}
```

The Tauri main binary is intentionally NOT part of this workspace
— it pulls `zk-sync-shell` as a path dependency and adds the GUI
framework on top. Keeping it outside the `cargo test --workspace`
job means the headless CI build doesn't need `webkit2gtk-4.1` /
Cocoa / WebView2 dev headers.

## Running the daemon locally

```bash
cd sdk
cargo build --release -p zk-sync-cli

export ZK_DRIVE_BASE_URL=https://drive.example.com
export ZK_DRIVE_BEARER=...           # bearer for the api client
export ZK_DRIVE_CATALOGUE_PATH=$HOME/.zk-sync/catalogue.db

./target/release/zk-sync run \
  --workspace <uuid> \
  --root /path/to/local/mirror
```

## CI

CI runs:

```
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace --no-fail-fast
```

See `.github/workflows/ci.yml` job `sdk`.
