# `zk-sync-sdk` — ZK Drive desktop sync SDK

A Rust workspace that provides the shared core for the ZK Drive
desktop sync daemon, the future Tauri tray UI, and any native
clients we layer on top (e.g. mobile via UniFFI). It is split into
five focused crates so each module can be vendored independently
into the Tauri shell or the eventual collab editor.

| Crate            | Purpose                                                                                                                                                                  |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `zk-sync-crypto` | XChaCha20-Poly1305 envelope that is byte-for-byte compatible with `zk-object-fabric/encryption/client_sdk` (Go). Per-chunk AAD, convergent nonce option.                 |
| `zk-sync-api`    | Typed HTTP / WebSocket client against ZK Drive: presigned upload/download, workspace change feed (built on PR #73), folder & file metadata.                              |
| `zk-sync-auth`   | OAuth2 PKCE (RFC 7636) + a coalescing `TokenSource` backed by the OS keychain (`keyring` crate).                                                                         |
| `zk-sync-engine` | Local SQLite catalogue, cross-platform filesystem watcher (`notify`), remote poller (cursor + WebSocket), reconciliation loop, last-writer-wins conflict policy.         |
| `zk-sync-cli`    | `zk-sync` daemon binary. Headless agent that wires the above crates together; the Tauri shell (phase 2) embeds the same crates and reuses the engine wiring 1:1.        |

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
│  └─ cli/                          # zk-sync-cli
│     └─ src/main.rs
```

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
