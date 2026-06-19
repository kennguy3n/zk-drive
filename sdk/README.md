# `zk-sync-sdk` — ZK Drive sync SDK

A Rust workspace that holds the shared sync core for every ZK Drive
client that mirrors a workspace to local storage: the headless
`zk-sync` daemon, the [Tauri desktop client](../desktop/README.md),
and — through the [mobile bridge](../docs/MOBILE_BRIDGE.md) — the
[Android](../docs/ANDROID_APP.md) and [iOS](../docs/MOBILE_IOS.md)
apps. The code is split into six focused crates so a host embeds only
the layers it needs and the GUI's heavy system dependencies stay out
of the headless build.

This document describes the crates, the public command/event surface
a GUI host drives, the change-feed sync model, and how to run the
daemon. Every claim is grounded in the crate sources cited inline as
`path:line` (paths are relative to this `sdk/` directory; server
paths are relative to the repository root).

## The six crates

| Crate | Source | Responsibility |
| --- | --- | --- |
| `zk-sync-crypto` | `crates/crypto/` | XChaCha20-Poly1305 chunked file envelope, byte-compatible with the Go client encryption SDK. Per-chunk AAD, optional convergent nonce. |
| `zk-sync-api` | `crates/api-client/` | Typed HTTP + WebSocket client: workspace change feed, presigned upload/download, file/folder metadata. |
| `zk-sync-auth` | `crates/auth/` | OAuth2 PKCE (RFC 7636) plus a coalescing token source backed by the OS keychain. |
| `zk-sync-engine` | `crates/sync-engine/` | Local SQLite catalogue, filesystem watcher, remote change-feed poller, and the reconciliation loop with a last-writer-wins conflict policy. |
| `zk-sync-shell` | `crates/desktop-shell/` | Multi-workspace harness. Owns the registry of bindings, persists it as a JSON sidecar, and exposes a serde-shaped `Command`/`ShellEvent` surface plus a cross-workspace tray aggregate. |
| `zk-sync-cli` | `crates/cli/` | The `zk-sync` daemon binary — a headless agent that wires the crates together. It is the integration surface CI builds against. |

The crates are layered. `cli` and `desktop-shell` both depend on
`sync-engine`, which in turn depends on `api-client`, `auth`, and
`crypto` (`Cargo.toml:12-18`). A host links the highest layer it needs
and gets the rest transitively.

```
            zk-sync-cli        zk-sync-shell
                  \             /     \
                   \           /       \  (GUI host: Tauri / Electron)
                    v         v
                  zk-sync-engine
                   /     |      \
                  v      v       v
         zk-sync-api  zk-sync-auth  zk-sync-crypto
```

The Tauri main binary is deliberately **not** a member of this
workspace (`Cargo.toml:23-26`). It pulls `zk-sync-shell` as a path
dependency and adds the GUI framework on top, which keeps the GUI's
heavy system dependencies (`webkit2gtk-4.1` and the like) out of the
headless `cargo test --workspace` job.

## `zk-sync-crypto` — the Strict-ZK file envelope

`strict_zk` files are encrypted on the client before they ever leave
the device; the server stores only ciphertext and never holds the
data-encryption key. This crate is the Rust implementation of that
envelope, and it is the mirror of the Go client encryption SDK so a
file sealed by a Rust client opens on a Go client and vice versa
(`crates/crypto/src/lib.rs:1-7`).

The wire format for each chunk is a fixed frame
(`crates/crypto/src/lib.rs:9-19`):

```text
| 24-byte XChaCha20-Poly1305 nonce | 4-byte big-endian ciphertext length | ciphertext (plaintext + 16-byte Poly1305 tag) |
```

- The content algorithm is `xchacha20-poly1305`
  (`crates/crypto/src/stream.rs:30-32`).
- Every chunk except the last carries exactly `DEFAULT_CHUNK_SIZE`
  bytes of plaintext — **16 MiB**, matching the Go SDK
  (`crates/crypto/src/stream.rs:16-18`).
- When a caller sets a per-object AAD prefix, the AEAD AAD for each
  chunk is `chunk_aad || "|" || big-endian uint64(chunk_index)`; an
  empty prefix means `AAD = nil`, preserving compatibility with
  objects sealed without AAD (`crates/crypto/src/aad.rs:1-22`,
  `src/lib.rs:21-34`).
- Convergent-nonce mode swaps random per-chunk nonces for
  deterministic content-derived nonces (HKDF-SHA256, info prefix
  `zkof-nonce-v1`), enabling intra-tenant dedup at the cost of forward
  secrecy. It is **off by default**
  (`crates/crypto/src/lib.rs:36-44`, `src/stream.rs:34-38`).

The public surface is two free functions plus the key/option types:
`encrypt_to_vec`, `decrypt_to_vec`, `Options { chunk_size,
convergent_nonce, chunk_aad }`, `DataEncryptionKey`,
`EncryptionEnvelope`, and `canonical_chunk_aad(...)`
(`crates/crypto/src/lib.rs:58-89`).

### Cross-language compatibility is a checked-in test

`crates/crypto/testdata/envelope-vectors.json` carries five
convergent-nonce ground-truth vectors produced by the Go SDK
(random-nonce vectors are omitted because they are not reproducible
across runs). The `go_compat` integration test asserts, for every
vector, that the Rust crate (1) produces **byte-identical** ciphertext
and (2) decrypts the Go-produced ciphertext back to the original
plaintext (`crates/crypto/tests/go_compat.rs:39-74`). A change to
either side's wire format must keep this fixture green.

## `zk-sync-api` — the typed backend client

The client carries the `Authorization: Bearer …` header on every
request by consulting a `TokenProvider` per call, so a freshly
refreshed token is picked up immediately by both HTTP and WebSocket
call sites — the bearer is never baked into a static header
(`crates/api-client/src/transport.rs:1-8`, `:82-105`). Build one with
`Client::builder(base_url)`, which normalises the base URL to end in
`/` so path joins always append (`:69-76`, `:141-167`). Authentication
is supplied as either a `StaticBearer` (tests, the CLI's bearer-file
flow) or a refreshing `TokenProvider` from the auth crate
(`:116-129`).

It exposes three sub-clients (`crates/api-client/src/lib.rs:1-32`):

- **`ChangefeedClient`** — `list_changes` walks
  `GET /api/changes?since={cursor}&limit={n}` for catch-up, and
  `stream_changes` opens `WS /api/ws?since={cursor}` for live updates.
  Neither path carries a workspace id: the caller's workspace is
  resolved server-side from the bearer token (the auth middleware reads
  it from the JWT claims), so one socket serves the bound workspace
  (`crates/api-client/src/changefeed.rs:147-181`, `:183-210`).
- **`StorageClient`** — `request_upload`
  (`POST /api/v1/files/{id}/uploads`) and `request_download`
  (`GET /api/v1/files/{id}/downloads`) negotiate presigned URLs for
  direct-to-storage transfer (`crates/api-client/src/storage.rs:52-102`).
- **`FsClient`** — `get_file`, `get_folder`, and `create_file` over
  `/api/v1/files` and `/api/v1/folders`
  (`crates/api-client/src/fs.rs:62-141`).

Both `File` and `Folder` carry an `encryption_mode` string. Sync
clients route on it: a `strict_zk` file is encrypted locally with the
crypto crate before the presigned PUT, while a `managed_encrypted`
file is uploaded as plaintext and the server seals it behind its KMS
envelope (`crates/api-client/src/fs.rs:22-25`,
`crates/api-client/src/storage.rs:1-7`). `managed_encrypted` is
therefore **not** zero-knowledge — the server can read those bytes;
only `strict_zk` keeps plaintext off the server entirely.

## `zk-sync-auth` — OAuth2 PKCE + keychain tokens

The agent authenticates with OAuth2 PKCE (RFC 7636). `PkceChallenge`
generates a 64-character base64url verifier and its SHA-256 (`S256`)
challenge; the verifier is replayed on the token exchange so the
server can recompute the challenge
(`crates/auth/src/pkce.rs:1-27`).

Tokens are modelled as a `TokenSet { access_token, refresh_token,
expires_at, scope }` that is considered expired **60 seconds early**
so the client refreshes before an upstream replica rejects the access
token (`crates/auth/src/token.rs:15-41`). `TokenSource` vends a valid
access token and, when the persisted one is inside the skew window,
refreshes it and writes the new bundle back to the store. Concurrent
callers share a single in-flight refresh through an `inflight` mutex,
so a burst of N tasks fires exactly one refresh request
(`crates/auth/src/token.rs:43-87`). `HttpRefresher` performs the
`grant_type=refresh_token` exchange against the backend token endpoint
(`crates/auth/src/token.rs:104-164`).

Persistence is the `TokenStore` trait. `KeychainStore` writes the
serialised `TokenSet` to the OS keychain via the `keyring` crate
(`Cargo.toml:80`) — Keychain on macOS, the Secret Service API on
Linux, Credential Manager on Windows — keyed by a `service` + `user`
pair, with keychain I/O offloaded to a blocking task and logout
treated as idempotent. `MemoryStore` is the in-process variant for
tests and ephemeral flows (`crates/auth/src/store.rs:1-19`, `:46-110`).

> For the identity provider, SSO, and OIDC internals behind these
> endpoints, see [`docs/IAM_CORE.md`](../docs/IAM_CORE.md). This crate
> only implements the client side of the flow.

## `zk-sync-engine` — the reconciliation core

The engine combines four parts (`crates/sync-engine/src/lib.rs:1-18`):

- a **`Catalogue`** (SQLite, `journal_mode=WAL` so readers don't block
  the single writer) tracking per file: remote id, last-known version,
  content hash, last-applied change-feed cursor, and a pinned flag for
  the offline cache (`crates/sync-engine/src/catalogue.rs:1-16`);
- a **`Watcher`** wrapping `notify` that emits coalesced `LocalEvent`s
  on a debounce interval;
- a **`RemotePoller`** that consumes the change feed and emits
  `RemoteEvent`s;
- an **`Engine::run`** loop that `select!`s over both event channels
  and reconciles them into upload / download / conflict actions
  (`crates/sync-engine/src/engine.rs:114-128`).

All blocking I/O is dispatched to `spawn_blocking`, so the engine is
safe to drive from a single-threaded runtime such as the Tauri main
thread (`crates/sync-engine/src/lib.rs:16-18`).

### Per-file sync status

Each catalogue row carries a `SyncStatus` with eight states
(`crates/sync-engine/src/catalogue.rs:19-56`): `up_to_date`,
`local_dirty`, `local_deleted`, `remote_dirty`, `remote_deleted`,
`conflict`, `in_flight`, and `evicted`. The status transition tables
are the heart of reconciliation — a local edit while a remote change
is pending escalates to `conflict` rather than clobbering either side
(`crates/sync-engine/src/catalogue.rs:91-143`).

### Conflict policy

The default policy is last-writer-wins: when both sides diverge, the
engine preserves the local copy as a `.conflict.<unix>` side file on
disk and then applies the remote version. If the local and remote
hashes already match, there is no real conflict and no side copy is
written (`crates/sync-engine/src/conflict.rs:1-66`). `ConflictPolicy`
is a trait, so a host can supply a different resolution strategy
(`crates/sync-engine/src/conflict.rs:20-29`).

### Engine-owned hidden directories

Inside each workspace root the engine keeps two hidden directories the
watcher is configured to ignore: `.zk-pending` for catalogue stubs of
remote files learned about but not yet downloaded, and `.zk-deleted`
for parking the paths of rows whose on-disk bytes were overwritten
(`crates/sync-engine/src/engine.rs:86-112`).

## The change-feed sync model

The change feed is a durable, monotonically-ordered log of every
mutation to a workspace's files, folders, and permissions. The same
JSON shape is used for both catch-up and live delivery, so a client's
reconciliation code is identical in both modes
(`internal/changefeed/changefeed.go:1-24`, `:70-92`).

A `Mutation` carries `sequence`, `workspace_id`, optional `actor_id`,
`kind`, `op`, `resource_id`, optional `parent_id`, `name`, optional
`metadata`, and `occurred_at`. `sequence` is always present because it
is the cursor; `parent_id`, `name`, and `metadata` are omitted when
empty (`internal/changefeed/changefeed.go:81-92`,
`crates/api-client/src/changefeed.rs:26-42`). The SDK names the kinds
`file`, `folder`, and `permission` and the ops `create`, `update`,
`rename`, `move`, `delete`; it keeps them as constants rather than a
closed enum so an unrecognised kind from the server does not fail JSON
decoding (`crates/api-client/src/changefeed.rs:44-60`).

A `RemotePoller` runs two strategies in series
(`crates/sync-engine/src/poller.rs:1-15`):

1. **Catch-up.** Page through `list_changes` from the persisted cursor
   until the server reports `has_more=false`, advancing the catalogue
   cursor after each page. The poller refuses to persist a cursor that
   regressed or failed to advance, so a misbehaving server can't rewind
   the client (`crates/sync-engine/src/poller.rs:121-200`).
2. **Live.** Open the WebSocket with `since={cursor}` to close the gap
   between catch-up and the handshake, persisting the cursor on every
   frame. If the socket drops, the poller falls back to catch-up and
   retries with exponential backoff. Catch-up and live use independent
   backoffs so a persistently broken WebSocket can't be papered over by
   a working HTTP path (`crates/sync-engine/src/poller.rs:53-119`,
   `:202-245`).

On the server side, the catch-up handler clamps `limit` to
`(0, MaxLimit]` (`MaxLimit = 500`) and uses a default page size when
`limit` is unset; an unset or negative `since` means "from the
beginning of retained history". A companion endpoint returns the
highest stored sequence so a client can learn "now" before going live
(`api/drive/changes.go:24-117`). The daemon's default `--page-size` of
500 matches the server's `MaxLimit` (`crates/cli/src/main.rs:75-78`).

## `zk-sync-shell` — driving the engine from a GUI host

The engine is single-workspace by design — its catalogue is bound to
one workspace at open time so the `files` table can never co-mingle
two workspaces' rows (`crates/desktop-shell/src/lib.rs:20-29`). The
shell wraps it in a multi-workspace harness whose entire surface is
two host-agnostic, serde-shaped contracts: a `Command` enum the
frontend sends, and a `ShellEvent` enum the shell broadcasts.

### Commands

`Command` is a `#[serde(tag = "type", content = "data")]` enum, so a
host can ferry a JSON blob straight to `App::dispatch` with no
per-variant glue (`crates/desktop-shell/src/command.rs:10-15`). The
variants (`crates/desktop-shell/src/command.rs:36-75`):

| Command | Effect |
| --- | --- |
| `AddWorkspace { workspace_id, label, root }` | Register a workspace. Idempotent for the same id + root; a different root returns `AlreadyRegistered`/`RootMismatch` rather than silently re-pointing it. |
| `RemoveWorkspace { workspace_id }` | Drop from the registry and stop any running task. The on-disk catalogue is **left intact** so a misclick is recoverable. |
| `RemoveLocalCache { workspace_id }` | Delete the workspace's catalogue file. Only valid when the workspace is not running. |
| `StartSync` / `StopSync { workspace_id }` | Start or stop the background sync loop. No-op if already in that state. |
| `GetStatus { workspace_id }` | Read one workspace's last-known `WorkspaceState`. |
| `ListWorkspaces` | Enumerate every workspace's state (frontends call this on launch). |
| `GetTrayState` | Read the cross-workspace tray aggregate. |

Replies come back as `CommandResult` (`Ok`, `Status`, `Workspaces`,
`Tray`) and failures as a typed `CommandError` the frontend can switch
on for a localised message (`crates/desktop-shell/src/command.rs:79-123`).
The command set is **append-only**: renaming a variant or its tag
would break already-deployed GUI builds.

### Events

`ShellEvent` is the same tagged-JSON shape and is also append-only
(`crates/desktop-shell/src/event.rs:18-70`): `WorkspaceAdded`,
`WorkspaceRemoved`, `HealthChanged`, `SummaryChanged`, `TrayChanged`,
and `TaskFailed`. The default `BroadcastSink` is a tokio broadcast
channel (capacity 32) that fans each event to every subscriber and
drops the oldest message on a slow consumer; the latest state is always
recoverable through `GetStatus`, so a dropped event never pins the GUI
on stale data (`crates/desktop-shell/src/event.rs:84-147`). A host that
wants custom delivery implements the `EventSink` trait itself.

### Health, summary, and tray state

`SyncHealth` is the coarse per-workspace lifecycle the tray renders —
`stopped`, `starting`, `idle`, `syncing`, `conflict`, `error` — as
distinct from the per-file `SyncStatus`
(`crates/desktop-shell/src/state.rs:6-49`). `Summary` is a
count-by-status snapshot (`total_files`, `total_bytes`, the eight
status counts, and the cursor) whose `pending()` total drives progress
bars and the "still syncing" test
(`crates/desktop-shell/src/summary.rs:10-49`). `TrayState` reduces
every workspace into one icon by priority — `Error → Conflict →
Syncing → Idle → Starting → Stopped` — and aggregates the pending and
conflict totals plus the first error message for the tooltip
(`crates/desktop-shell/src/tray.rs:1-105`).

### Persistent registry

`App` persists its registry as a JSON sidecar (`AppConfig` of
`WorkspaceEntry { workspace_id, label, root, catalogue_path,
autostart }`) so workspaces survive a relaunch. The file is rewritten
with the write-tmp-then-rename pattern so a crash mid-write never
corrupts the registry, and an unrecognised on-disk schema is rejected
loudly rather than silently coerced
(`crates/desktop-shell/src/config.rs:1-109`).

### A minimal host

```rust
use std::sync::Arc;
use zk_sync_api::{Bearer, Client};
use zk_sync_shell::{App, BroadcastSink, Command, EventSink};

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

    // Forward shell events onto the GUI's own bus.
    let mut rx = sink.subscribe();
    tokio::spawn(async move {
        while let Ok(ev) = rx.recv().await {
            // Tauri:    window.emit("sync", &ev)?;
            // Electron: webContents.send("sync", &ev);
            tracing::info!("shell event: {ev:?}");
        }
    });

    // Dispatch a user action.
    app.dispatch(Command::AddWorkspace {
        workspace_id: uuid::Uuid::new_v4(),
        label: "Northwind Trading".into(),
        root: "/Users/alice/Northwind Trading".into(),
    })
    .await?;

    tokio::signal::ctrl_c().await?;
    Ok(())
}
```

The [Tauri desktop client](../desktop/README.md) is exactly this host
with a window and a system tray bolted on; its IPC commands map 1:1 to
the `Command` variants above.

## `zk-sync-cli` — the headless daemon

The `zk-sync` binary is the integration surface CI exercises end to
end (`crates/cli/src/main.rs:9-11`; binary name set in
`crates/cli/Cargo.toml:11-13`). It takes global flags for the backend
URL, the catalogue path, and the bearer source, plus two subcommands
(`crates/cli/src/main.rs:26-79`):

- **`status --workspace <uuid>`** — a purely local query that prints
  the persisted cursor and tracked-file count without needing a token,
  so an operator can diagnose state on a box that has lost its
  credentials (`crates/cli/src/main.rs:203-212`).
- **`run --workspace <uuid> --root <path> [--page-size 500]`** —
  starts the watcher, poller, and engine and runs the sync loop until
  `SIGINT` (`crates/cli/src/main.rs:213-262`).

The bearer token is read from `--bearer-file` (or `ZK_DRIVE_BEARER_FILE`)
or the `ZK_DRIVE_BEARER` environment variable, with the file taking
precedence. The token is deliberately never accepted as a command-line
argument, because argv is world-readable via `/proc/<pid>/cmdline` on
Linux (`crates/cli/src/main.rs:39-56`, `:112-150`).

```bash
cd sdk
cargo build --release -p zk-sync-cli

export ZK_DRIVE_BASE_URL=https://drive.example.com
export ZK_DRIVE_CATALOGUE_PATH="$HOME/.zk-sync/northwind.db"
export ZK_DRIVE_BEARER=...        # bearer for the Northwind workspace

# Local-only health check (no network):
./target/release/zk-sync status --workspace <northwind-workspace-uuid>

# Mirror the Northwind Trading workspace into a local folder:
./target/release/zk-sync run \
  --workspace <northwind-workspace-uuid> \
  --root "$HOME/Northwind Trading"
```

The catalogue is bound to one workspace for its lifetime, so reusing
one `--catalogue` path across different `--workspace` values is
rejected rather than silently co-mingling rows
(`crates/cli/src/main.rs:183-193`).

## Build, test, and CI

```bash
cd sdk
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace --no-fail-fast
```

These are exactly the three steps the `sdk` job runs in
`.github/workflows/ci.yml` ("Rust desktop SDK (fmt, clippy, test)")
on a pinned Rust 1.86 toolchain, with `working-directory: sdk`
(`.github/workflows/ci.yml:114-148`). The workspace edition and
`rust-version = "1.86"` are declared in `Cargo.toml:39-43`.
`rusqlite`'s `bundled` feature compiles SQLite from source, so the
test job needs no system SQLite (`.github/workflows/ci.yml:133-135`).
Because the Tauri main binary lives outside this workspace, the test
job needs none of the GUI system headers.

## Layout

```
sdk/
├─ Cargo.toml                       # workspace manifest
└─ crates/
   ├─ crypto/                       # zk-sync-crypto
   │  ├─ src/{lib,error,envelope,aad,stream}.rs
   │  ├─ testdata/envelope-vectors.json
   │  └─ tests/go_compat.rs         # cross-language fixture test
   ├─ api-client/                   # zk-sync-api
   │  └─ src/{lib,error,transport,fs,storage,changefeed}.rs
   ├─ auth/                         # zk-sync-auth
   │  └─ src/{lib,pkce,token,store}.rs
   ├─ sync-engine/                  # zk-sync-engine
   │  └─ src/{lib,catalogue,events,hash,watcher,poller,conflict,engine}.rs
   ├─ desktop-shell/                # zk-sync-shell
   │  ├─ src/{lib,app,command,config,event,state,summary,tray}.rs
   │  └─ tests/dispatch_integration.rs
   └─ cli/                          # zk-sync-cli (binary: zk-sync)
      └─ src/main.rs
```
